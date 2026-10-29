package moderation

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func feedWith(t *testing.T, domains ...string) *PhishFeed {
	t.Helper()
	f := NewPhishFeed(slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.set.Store(newDomainSet(domains...))
	return f
}

func TestParseDomainsJSONStreams(t *testing.T) {
	var got []string
	body := `{"updated":"2026-01-01","domains":["a.example","b.example"],"extra":{"x":[1,2]}}`
	if err := parseDomainsJSON(strings.NewReader(body), func(d string) { got = append(got, d) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a.example,b.example" {
		t.Fatalf("got %v", got)
	}
	if err := parseDomainsJSON(strings.NewReader(`[1,2]`), func(string) {}); err == nil {
		t.Fatal("a JSON array was accepted as a feed")
	}
}

func TestDomainSetDedupes(t *testing.T) {
	s := newDomainSet("a.example", "b.example", "a.example")
	if len(s.hashes) != 2 || !s.has("a.example") || s.has("c.example") {
		t.Fatalf("hashes=%d has(a)=%v has(c)=%v", len(s.hashes), s.has("a.example"), s.has("c.example"))
	}
}

func TestPhishFeedMatch(t *testing.T) {
	f := feedWith(t, "free-nitro.gift", "steamcommunity.com")
	cases := map[string]bool{
		"free-nitro.gift":            true,
		"login.free-nitro.gift":      true,
		"FREE-NITRO.GIFT":            true,
		"notfree-nitro.gift":         false,
		"discord.com":                false,
		"steamcommunity.com":         false, // allowlisted even if a feed lists it
		"store.steamcommunity.com":   false,
		"free-nitro.gift.example.ro": false,
	}
	for host, want := range cases {
		if _, got := f.Match(host); got != want {
			t.Errorf("Match(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLinkHost(t *testing.T) {
	cases := map[string]string{
		"https://Free-Nitro.gift/claim?x=1": "free-nitro.gift",
		"www.example.com/path":              "www.example.com",
		"http://a.b.c:8080/":                "a.b.c",
	}
	for in, want := range cases {
		if got := strings.ToLower(linkHost(in)); got != want {
			t.Errorf("linkHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeDomainRejectsNonDomains(t *testing.T) {
	for _, bad := range []string{"bit.ly/abc", "nodot", "*.evil.com", "a b.com", ""} {
		if got := normalizeDomain(bad); got != "" {
			t.Errorf("normalizeDomain(%q) = %q, want empty", bad, got)
		}
	}
}

func TestRefreshMergesAndKeepsGoodListOnFailure(t *testing.T) {
	var lines strings.Builder
	for i := range feedMinDomains {
		fmt.Fprintf(&lines, "scam%d.example\n", i)
	}
	lines.WriteString("discord.com\nbit.ly/xyz\n")

	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.URL.Path == "/json" {
			_, _ = io.WriteString(w, `{"domains":["json-scam.example"]}`)
			return
		}
		_, _ = io.WriteString(w, lines.String())
	}))
	defer srv.Close()

	saved := phishFeeds
	defer func() { phishFeeds = saved }()
	phishFeeds = phishFeeds[:0:0]
	phishFeeds = append(phishFeeds,
		struct {
			url   string
			parse func(io.Reader, func(string)) error
		}{srv.URL + "/txt", parseLines},
		struct {
			url   string
			parse func(io.Reader, func(string)) error
		}{srv.URL + "/json", parseDomainsJSON},
	)

	f := NewPhishFeed(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := f.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Size() != feedMinDomains+1 {
		t.Fatalf("size = %d, want %d (allowlisted and path entries dropped)", f.Size(), feedMinDomains+1)
	}
	if _, ok := f.Match("json-scam.example"); !ok {
		t.Fatal("JSON feed not merged")
	}

	fail = true
	if err := f.Refresh(context.Background()); err == nil {
		t.Fatal("refresh with every source down reported success")
	}
	if f.Size() != feedMinDomains+1 {
		t.Fatal("failed refresh replaced the good list")
	}
}
