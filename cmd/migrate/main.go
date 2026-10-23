// Command migrate is a one-shot tool that reconstructs the email history from
// the bot's own records and writes it into Postgres.
//
// It has two read sources, neither of which involves reversing a hash:
//
//   - the moderator log channel, whose "submitted" lines pair the submitting
//     member with the SHA-256 of the address, and whose "already used" notices
//     pair a plaintext address with that same hash;
//   - the old Mongo `usedEmails` collection, the set of hashes that were
//     actually verified.
//
// Only hashes confirmed as used — present in Mongo, or named in an "already
// used" notice — are imported; a submission alone may have been abandoned, and
// importing it would lock that member out of their own address. An owner is
// recorded only when exactly one member ever submitted the hash. Hashes with
// no recovered plaintext are imported as blocking legacy entries.
//
// It is --dry-run by default and prints a full reconciliation report; pass
// --commit to write.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"steki-bot/config"
	"steki-bot/store"
)

// These patterns match the exact Greek phrases the bot has historically
// logged. The "received code" line is deliberately NOT matched: it logs
// sha256(code+salt), which is not an email hash and must never be treated as
// one.
var (
	hashRe     = regexp.MustCompile(`[0-9a-fA-F]{64}`)
	userIDRe   = regexp.MustCompile(`<@!?(\d+)>`)
	emailRe    = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@(?:edu\.hmu\.gr|csd\.uoc\.gr)`)
	submittedP = regexp.MustCompile(`έστειλε mail για εγγραφή`)
	alreadyP   = regexp.MustCompile(`έχει ήδη χρησιμοποιηθεί`)
)

// recovered accumulates what the records say about one email hash.
type recovered struct {
	hash       string
	email      string              // plaintext, when a notice named it and it hashes to hash
	submitters map[string]struct{} // members who submitted an address with this hash
	used       bool                // confirmed verified: in Mongo or in an "already used" notice
}

func entry(byHash map[string]*recovered, hash string) *recovered {
	r := byHash[hash]
	if r == nil {
		r = &recovered{hash: hash, submitters: map[string]struct{}{}}
		byHash[hash] = r
	}
	return r
}

// owner returns the member the hash belongs to, when the records make that
// unambiguous. Only submitters were ever sent a code for the address, so a used
// hash with exactly one submitter was verified by that member. With several,
// the records cannot say which of them finished, and guessing could bind the
// address to the wrong account.
func (r *recovered) owner() string {
	if len(r.submitters) != 1 {
		return ""
	}
	for id := range r.submitters {
		return id
	}
	return ""
}

func main() {
	var (
		dryRun = flag.Bool("dry-run", true, "report only; do not write")
		commit = flag.Bool("commit", false, "write recovered records to Postgres")
		limit  = flag.Int("max-messages", 0, "stop after scanning this many log messages (0 = all)")
	)
	flag.Parse()
	if *commit {
		*dryRun = false
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		fatal(logger, "config", err)
	}
	mongoURL := os.Getenv("MONGO_URL") // only this tool still needs it

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	byHash := map[string]*recovered{}

	// 1. Scrape the log channel.
	scanned, err := scrapeLogChannel(ctx, logger, cfg.DiscordToken, config.LogChannelID, *limit, byHash)
	if err != nil {
		fatal(logger, "scrape log channel", err)
	}
	logger.Info("log channel scanned", "messages", scanned, "hashes_seen", len(byHash))

	// 2. Fold in the Mongo hash set, if configured.
	var mongoCount int
	if mongoURL != "" {
		mongoCount, err = loadMongoHashes(ctx, mongoURL, byHash)
		if err != nil {
			fatal(logger, "load mongo hashes", err)
		}
		logger.Info("mongo hashes loaded", "count", mongoCount)
	} else {
		logger.Warn("MONGO_URL not set; only hashes named in \"already used\" notices count as verified")
	}

	// 3. Classify. Unconfirmed submissions are reported but never written.
	var withEmail, withOwner, ambiguous, unconfirmed int
	resolved := make([]*recovered, 0, len(byHash))
	for _, r := range byHash {
		if !r.used {
			unconfirmed++
			continue
		}
		resolved = append(resolved, r)
		if r.email != "" {
			withEmail++
		}
		switch {
		case r.owner() != "":
			withOwner++
		case len(r.submitters) > 1:
			ambiguous++
		}
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].hash < resolved[j].hash })

	fmt.Println("================ reconciliation report ================")
	fmt.Printf("log messages scanned          : %d\n", scanned)
	fmt.Printf("mongo hashes loaded           : %d\n", mongoCount)
	fmt.Printf("distinct hashes seen          : %d\n", len(byHash))
	fmt.Printf("  skipped, never confirmed    : %d  (submitted but not known to be verified)\n", unconfirmed)
	fmt.Printf("  to import                   : %d\n", len(resolved))
	fmt.Printf("    plaintext recovered       : %d  (verified_emails; the rest legacy_email_hashes)\n", withEmail)
	fmt.Printf("    owner known               : %d\n", withOwner)
	fmt.Printf("    owner ambiguous, left out : %d  (several members submitted it)\n", ambiguous)
	fmt.Println("=======================================================")

	if *dryRun {
		fmt.Println("\nDRY RUN — nothing written. Re-run with --commit to apply.")
		for n, r := range resolved {
			if n >= 15 {
				fmt.Printf("… and %d more\n", len(resolved)-15)
				break
			}
			fmt.Printf("  %s  email=%q  owner=%q\n", short(r.hash), r.email, r.owner())
		}
		return
	}

	// 4. Write.
	st, err := store.Open(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		fatal(logger, "open database", err)
	}
	defer st.Close()

	var wroteEmail, wroteHash int
	for _, r := range resolved {
		var owner *string
		if id := r.owner(); id != "" {
			owner = &id
		}

		if r.email != "" {
			if err := st.AddVerifiedEmail(ctx, r.email, owner, store.SourceDiscordLog); err != nil {
				logger.Error("write verified email failed", "hash", short(r.hash), "error", err)
				continue
			}
			if owner != nil {
				if err := st.EnsureMember(ctx, *owner, ""); err != nil {
					logger.Warn("ensure member failed", "user_id", *owner, "error", err)
				}
			}
			wroteEmail++
			continue
		}

		if err := st.AddLegacyHash(ctx, r.hash, owner, store.SourceDiscordLog); err != nil {
			logger.Error("write legacy hash failed", "hash", short(r.hash), "error", err)
			continue
		}
		wroteHash++
	}

	fmt.Printf("\nCOMMITTED: %d verified emails, %d legacy hashes.\n", wroteEmail, wroteHash)
}

// scrapeLogChannel pages backward through the log channel, extracting hash
// associations from the two relevant message shapes.
func scrapeLogChannel(ctx context.Context, logger *slog.Logger, token, channelID string,
	max int, byHash map[string]*recovered) (int, error) {

	// REST only: no gateway connection is needed to read history.
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		return 0, err
	}

	var scanned int
	before := ""

	for {
		if err := ctx.Err(); err != nil {
			return scanned, err
		}
		msgs, err := sess.ChannelMessages(channelID, 100, before, "", "", discordgo.WithContext(ctx))
		if err != nil {
			return scanned, err
		}
		if len(msgs) == 0 {
			break
		}

		for _, m := range msgs {
			scanned++
			ingestLine(m.Content, byHash)
			before = m.ID
		}

		if max > 0 && scanned >= max {
			break
		}
		time.Sleep(300 * time.Millisecond) // stay well under the rate limit
	}
	return scanned, nil
}

// ingestLine pulls a hash, and whatever identity context is present, from one
// log message and merges it into byHash.
func ingestLine(content string, byHash map[string]*recovered) {
	hash := hashRe.FindString(content)
	if hash == "" {
		return
	}
	submitted := submittedP.MatchString(content)
	alreadyUsed := alreadyP.MatchString(content)
	if !submitted && !alreadyUsed {
		// Notably the "received code" line, whose hash is sha256(code+salt).
		return
	}
	r := entry(byHash, normalizeHex(hash))

	if submitted {
		// Only a submission line's mention is trustworthy as a claimant; an
		// "already used" notice names whoever tried the address later.
		if m := userIDRe.FindStringSubmatch(content); m != nil {
			r.submitters[m[1]] = struct{}{}
		}
	}
	if alreadyUsed {
		r.used = true
		// Accept the plaintext only if it really is the address behind the
		// hash, which guards against a malformed or mis-paired line.
		if email := emailRe.FindString(content); email != "" && r.email == "" &&
			store.LegacyHash(email) == r.hash {
			r.email = store.NormalizeEmail(email)
		}
	}
}

// loadMongoHashes folds the old usedEmails set into byHash without overwriting
// anything the richer log scrape already resolved.
func loadMongoHashes(ctx context.Context, uri string, byHash map[string]*recovered) (int, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return 0, err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	cur, err := client.Database("steki").Collection("usedEmails").Find(ctx, bson.M{})
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)

	var count int
	for cur.Next(ctx) {
		var doc struct {
			Email string `bson:"email"`
		}
		if err := cur.Decode(&doc); err != nil {
			return count, err
		}
		if doc.Email == "" {
			continue
		}
		count++
		entry(byHash, normalizeHex(doc.Email)).used = true
	}
	return count, cur.Err()
}

func normalizeHex(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'F' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12] + "…"
}

func fatal(logger *slog.Logger, stage string, err error) {
	logger.Error("migration failed", "stage", stage, "error", err)
	os.Exit(1)
}
