package moderation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/maphash"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/net/idna"
)

// phishFeeds are public, community-maintained lists of Discord phishing and
// scam domains. They are merged, so one going stale or offline only narrows
// coverage instead of disabling it.
//
// Each parser streams: it calls emit once per domain instead of returning a
// slice, so a refresh never holds the raw lists in memory.
var phishFeeds = []struct {
	url   string
	parse func(r io.Reader, emit func(string)) error
}{
	{"https://raw.githubusercontent.com/Discord-AntiScam/scam-links/main/list.txt", parseLines},
	{"https://raw.githubusercontent.com/nikolaischunk/discord-phishing-links/main/domain-list.json", parseDomainsJSON},
}

const (
	feedMaxBytes = 16 << 20
	// feedMinDomains rejects a refresh that came back implausibly small, so
	// a truncated or emptied feed never replaces a good list.
	feedMinDomains = 1000
)

// neverPhishing are domains no feed entry may ever block. A single poisoned
// or mistaken entry for one of these would delete every link to it.
var neverPhishing = map[string]bool{
	"discord.com": true, "discord.gg": true, "discordapp.com": true, "discordapp.net": true,
	"discord.media": true, "discord.new": true, "youtube.com": true, "youtu.be": true,
	"google.com": true, "github.com": true, "githubusercontent.com": true, "tenor.com": true,
	"imgur.com": true, "wikipedia.org": true, "steampowered.com": true, "steamcommunity.com": true,
	"twitch.tv": true, "x.com": true, "twitter.com": true, "reddit.com": true,
	"hmu.gr": true, "uoc.gr": true, "spotify.com": true, "instagram.com": true,
}

// PhishFeed holds the merged phishing domain set, refreshed in the background.
type PhishFeed struct {
	set    atomic.Pointer[domainSet]
	client *http.Client
	log    *slog.Logger
}

// domainSet is the loaded domains as a sorted list of 64-bit hashes, eight
// bytes a domain where a map of strings costs several times that and keeps
// every name alive. The hash is seeded per process, so nobody can pick a
// domain that collides with a listed one; at tens of thousands of entries an
// accidental collision is a one in 10^14 event.
type domainSet struct {
	seed   maphash.Seed
	hashes []uint64
}

func newDomainSet(domains ...string) *domainSet {
	s := &domainSet{seed: maphash.MakeSeed()}
	for _, d := range domains {
		s.hashes = append(s.hashes, maphash.String(s.seed, d))
	}
	s.finish()
	return s
}

// finish sorts and deduplicates, and releases the slack left by appending.
func (s *domainSet) finish() {
	slices.Sort(s.hashes)
	s.hashes = slices.Clip(slices.Compact(s.hashes))
}

func (s *domainSet) has(domain string) bool {
	_, ok := slices.BinarySearch(s.hashes, maphash.String(s.seed, domain))
	return ok
}

// NewPhishFeed returns an empty feed; call Refresh to load it.
func NewPhishFeed(logger *slog.Logger) *PhishFeed {
	return &PhishFeed{client: &http.Client{Timeout: 30 * time.Second}, log: logger}
}

// Size reports how many domains are loaded.
func (f *PhishFeed) Size() int {
	if d := f.set.Load(); d != nil {
		return len(d.hashes)
	}
	return 0
}

// Refresh downloads every feed and swaps in the merged set. Sources that fail
// are skipped; if the merged set is implausibly small the previous one is kept.
func (f *PhishFeed) Refresh(ctx context.Context) error {
	merged := &domainSet{seed: maphash.MakeSeed()}
	for _, src := range phishFeeds {
		// A source that fails half way keeps the entries it already yielded:
		// they are a prefix of a real list, so each is still a real entry.
		err := f.fetch(ctx, src.url, src.parse, func(d string) {
			if d = normalizeDomain(d); d != "" && !neverPhishing[d] {
				merged.hashes = append(merged.hashes, maphash.String(merged.seed, d))
			}
		})
		if err != nil {
			f.log.Warn("phishing feed fetch failed", "url", src.url, "error", err)
		}
	}
	merged.finish()
	if len(merged.hashes) < feedMinDomains {
		return fmt.Errorf("phishing feed: only %d domains, keeping %d", len(merged.hashes), f.Size())
	}
	f.set.Store(merged)
	f.log.Info("phishing feed refreshed", "domains", len(merged.hashes))
	return nil
}

func (f *PhishFeed) fetch(ctx context.Context, u string, parse func(io.Reader, func(string)) error, emit func(string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	res, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	return parse(io.LimitReader(res.Body, feedMaxBytes), emit)
}

// Match reports the listed domain that host falls under, checking the host
// itself and each parent domain, so "login.evil.com" matches "evil.com".
func (f *PhishFeed) Match(host string) (string, bool) {
	d := f.set.Load()
	if d == nil {
		return "", false
	}
	host = normalizeDomain(host)
	// Trusted domains cover their subdomains too, so a feed entry for
	// "x.google.com" can never block a link to Google.
	for h := host; h != "" && strings.Contains(h, "."); {
		if neverPhishing[h] {
			return "", false
		}
		_, h, _ = strings.Cut(h, ".")
	}
	for host != "" && strings.Contains(host, ".") {
		if d.has(host) {
			return host, true
		}
		_, host, _ = strings.Cut(host, ".")
	}
	return "", false
}

// normalizeDomain lowercases a bare domain and rejects anything that is not
// one: entries with paths, wildcards or no dot are skipped.
func normalizeDomain(d string) string {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	if !strings.Contains(d, ".") {
		return ""
	}
	for _, r := range d {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return ""
		}
	}
	return d
}

// linkHost extracts the host from a link matched by urlRegex, which may lack
// a scheme ("www.example.com/x") and may end in punctuation from the
// surrounding text. Internationalised hosts are returned in punycode, the form
// the phishing feeds list them in.
func linkHost(link string) string {
	if !strings.Contains(link, "://") {
		link = "https://" + link
	}
	rest := link[strings.Index(link, "://")+3:]
	// The authority ends at the first of / ? # or \; an @ after that point
	// belongs to the path, not to userinfo.
	if end := strings.IndexAny(rest, "/?#\\"); end >= 0 {
		rest = rest[:end]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:] // userinfo: "https://discord.com@evil.com" opens evil.com
	}
	end := strings.IndexFunc(rest, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) || r == '.' || r == '-')
	})
	if end >= 0 {
		rest = rest[:end]
	}
	host, err := idna.Punycode.ToASCII(strings.ToLower(strings.Trim(rest, ".")))
	if err != nil {
		return ""
	}
	return host
}

func parseLines(r io.Reader, emit func(string)) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			emit(line)
		}
	}
	return sc.Err()
}

// parseDomainsJSON reads {"domains": ["a.example", ...]} one element at a
// time, so the array never exists as a whole.
func parseDomainsJSON(r io.Reader, emit func(string)) error {
	dec := json.NewDecoder(r)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return fmt.Errorf("feed is not a JSON object: %v", err)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		if key != "domains" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return fmt.Errorf("domains is not an array: %v", err)
		}
		for dec.More() {
			var d string
			if err := dec.Decode(&d); err != nil {
				return err
			}
			emit(d)
		}
		if _, err := dec.Token(); err != nil { // the closing ]
			return err
		}
	}
	return nil
}
