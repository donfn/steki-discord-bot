// Package legacylog reads the email history the old bot left in its moderator
// log channel, and decides from it who each imported email hash belongs to.
//
// The old bot stored only unsalted SHA-256 hashes of addresses, with no link to
// the member who verified them. Its log lines are the only record of that
// link, in two shapes:
//
//   - "Ο χρήστης <@id> έστειλε (το εξής) mail για εγγραφή: ```Hash: h```", a
//     member submitting an address;
//   - "Το email έχει ήδη χρησιμοποιηθεί … h", the bot refusing an address that
//     was already registered, which names the plaintext address.
//
// "Ο χρήστης <@id> έλαβε (στο inbox του) τον κωδικό εγγραφής ```Hash: h```" is
// deliberately not read: that hash is of the verification code, not of an
// address. Nothing here reverses a hash; plaintext is only ever taken from a
// line that printed it, and only if it hashes to the hash beside it.
package legacylog

import (
	"regexp"
	"strings"
	"time"

	"steki-bot/store"
)

var (
	hashRe     = regexp.MustCompile(`[0-9a-fA-F]{64}`)
	userIDRe   = regexp.MustCompile(`<@!?(\d+)>`)
	emailRe    = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@(?:edu\.hmu\.gr|csd\.uoc\.gr)`)
	submittedP = regexp.MustCompile(`έστειλε (?:το εξής )?mail για εγγραφή`)
	inUseP     = regexp.MustCompile(`έχει ήδη χρησιμοποιηθεί`)
)

// Parse extracts the evidence in one log message, if it holds any.
func Parse(messageID, content string, posted time.Time) (store.LogEvidence, bool) {
	hash := strings.ToLower(hashRe.FindString(content))
	if hash == "" {
		return store.LogEvidence{}, false
	}
	e := store.LogEvidence{MessageID: messageID, Hash: hash, PostedAt: posted}

	switch {
	case submittedP.MatchString(content):
		m := userIDRe.FindStringSubmatch(content)
		if m == nil {
			return store.LogEvidence{}, false
		}
		e.Kind, e.UserID = store.EvidenceSubmitted, m[1]
	case inUseP.MatchString(content):
		e.Kind = store.EvidenceInUse
		// Keep the plaintext only if it really is the address behind the
		// hash, which guards against a malformed or mis-paired line.
		if email := emailRe.FindString(content); email != "" && store.LegacyHash(email) == hash {
			e.Email = store.NormalizeEmail(email)
		}
	default:
		return store.LogEvidence{}, false
	}
	return e, true
}

// triedWithin is how soon before an "in use" notice a submission must be for
// it to be the attempt that notice refused. The old bot logged the two lines
// back to back.
const triedWithin = 2 * time.Minute

// Resolve decides, from all the evidence for one hash, who the address belongs
// to and what it is in plaintext. Either may be empty.
//
// Every "in use" notice refused someone, and that someone submitted the
// address just before it; those submissions are set aside, since a refused
// member cannot be the owner. The owner is then whoever else submitted the
// address, provided exactly one member did. With two or more the records
// cannot say which finished, and guessing could bind the address to the wrong
// account, so the owner is left unknown.
func Resolve(evidence []store.LogEvidence) (owner, email string) {
	refused := map[string]bool{} // message IDs of refused submissions
	for _, n := range evidence {
		if n.Kind != store.EvidenceInUse {
			continue
		}
		if n.Email != "" && email == "" {
			email = n.Email
		}
		var last *store.LogEvidence
		for i := range evidence {
			s := &evidence[i]
			if s.Kind != store.EvidenceSubmitted || refused[s.MessageID] ||
				s.PostedAt.After(n.PostedAt) || n.PostedAt.Sub(s.PostedAt) > triedWithin {
				continue
			}
			if last == nil || s.PostedAt.After(last.PostedAt) {
				last = s
			}
		}
		if last != nil {
			refused[last.MessageID] = true
		}
	}

	candidates := map[string]bool{}
	for _, s := range evidence {
		if s.Kind == store.EvidenceSubmitted && !refused[s.MessageID] {
			candidates[s.UserID] = true
		}
	}
	if len(candidates) == 1 {
		for id := range candidates {
			owner = id
		}
	}
	return owner, email
}
