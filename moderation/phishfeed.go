package moderation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/net/idna"
)

// phishFeeds are public, community-maintained lists of Discord phishing and
// scam domains. They are merged, so one going stale or offline only narrows
// coverage instead of disabling it.
var phishFeeds = []struct {
	url   string
	parse func(io.Reader) ([]string, error)
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
	domains atomic.Pointer[map[string]struct{}]
	client  *http.Client
	log     *slog.Logger
}

// NewPhishFeed returns an empty feed; call Refresh to load it.
func NewPhishFeed(logger *slog.Logger) *PhishFeed {
	return &PhishFeed{client: &http.Client{Timeout: 30 * time.Second}, log: logger}
}

// Size reports how many domains are loaded.
func (f *PhishFeed) Size() int {
	if d := f.domains.Load(); d != nil {
		return len(*d)
	}
	return 0
}

// Refresh downloads every feed and swaps in the merged set. Sources that fail
// are skipped; if the merged set is implausibly small the previous one is kept.
func (f *PhishFeed) Refresh(ctx context.Context) error {
	merged := map[string]struct{}{}
	for _, src := range phishFeeds {
		domains, err := f.fetch(ctx, src.url, src.parse)
		if err != nil {
			f.log.Warn("phishing feed fetch failed", "url", src.url, "error", err)
			continue
		}
		for _, d := range domains {
			if d = normalizeDomain(d); d != "" && !neverPhishing[d] {
				merged[d] = struct{}{}
			}
		}
	}
	if len(merged) < feedMinDomains {
		return fmt.Errorf("phishing feed: only %d domains, keeping %d", len(merged), f.Size())
	}
	f.domains.Store(&merged)
	f.log.Info("phishing feed refreshed", "domains", len(merged))
	return nil
}

func (f *PhishFeed) fetch(ctx context.Context, u string, parse func(io.Reader) ([]string, error)) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	return parse(io.LimitReader(res.Body, feedMaxBytes))
}

// Match reports the listed domain that host falls under, checking the host
// itself and each parent domain, so "login.evil.com" matches "evil.com".
func (f *PhishFeed) Match(host string) (string, bool) {
	d := f.domains.Load()
	if d == nil {
		return "", false
	}
	host = normalizeDomain(host)
	for host != "" && strings.Contains(host, ".") {
		if neverPhishing[host] {
			return "", false
		}
		if _, ok := (*d)[host]; ok {
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
	if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
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

func parseLines(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func parseDomainsJSON(r io.Reader) ([]string, error) {
	var body struct {
		Domains []string `json:"domains"`
	}
	err := json.NewDecoder(r).Decode(&body)
	return body.Domains, err
}
