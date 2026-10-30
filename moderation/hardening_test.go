package moderation

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/net/idna"
)

// Lookalike domains in non-Latin script must still be seen as links and
// matched against the feed, which lists them in punycode.
func TestLookalikeDomainsAreLinksAndMatch(t *testing.T) {
	lookalike := "dіscord-gift.com" // Cyrillic і
	puny, err := idna.Punycode.ToASCII(lookalike)
	if err != nil || puny == lookalike {
		t.Fatalf("punycode = %q, %v", puny, err)
	}
	text := "δωρεάν nitro https://" + lookalike + "/claim"
	if !ContainsURL(text) {
		t.Fatal("lookalike link not detected")
	}
	f := feedWith(t, puny)
	links := urlRegex.FindAllString(text, -1)
	if _, ok := f.Match(linkHost(links[0])); !ok {
		t.Fatalf("lookalike %q (host %q) not matched", links[0], linkHost(links[0]))
	}
}

func TestLinkHostTricks(t *testing.T) {
	cases := map[string]string{
		"https://discord.com@evil.com/login": "evil.com", // userinfo: the browser opens evil.com
		"https://evil.com).":                 "evil.com", // punctuation from a masked link or sentence
		"https://EVIL.com:443/x":             "evil.com",
		"https://ευ.gr/x":                    "xn--qxa6a.gr",
	}
	for in, want := range cases {
		if got := linkHost(in); got != want {
			t.Errorf("linkHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTextIncludesForwardedContent(t *testing.T) {
	m := &discordgo.Message{
		Content: "",
		MessageSnapshots: []discordgo.MessageSnapshot{{Message: &discordgo.Message{
			Content:     "https://scam.example/free",
			Attachments: []*discordgo.MessageAttachment{{ID: "a"}},
		}}},
	}
	if !ContainsURL(Text(m)) {
		t.Error("forwarded link not visible to checks")
	}
	if len(Attachments(m)) != 1 {
		t.Error("forwarded attachment not visible to checks")
	}
}

// A member cannot close the bot's code block and smuggle @everyone out of it.
func TestQuoteNeutralisesCodeBlockBreakout(t *testing.T) {
	q := Quote("x```\n@everyone free nitro")
	if strings.Contains(q, "```") {
		t.Fatalf("quoted text still closes the block: %q", q)
	}
	if len(SafeMentions.Parse) != 1 || SafeMentions.Parse[0] != discordgo.AllowedMentionTypeUsers {
		t.Fatalf("SafeMentions allows more than user pings: %+v", SafeMentions.Parse)
	}
}

func TestLinkHostIgnoresAtSignAfterAuthority(t *testing.T) {
	for in, want := range map[string]string{
		"https://evil.com?@discord.com":  "evil.com",
		"https://evil.com#@discord.com":  "evil.com",
		"https://evil.com\\@discord.com": "evil.com",
	} {
		if got := linkHost(in); got != want {
			t.Errorf("linkHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrustedDomainSubdomainsNeverMatch(t *testing.T) {
	f := feedWith(t, "x.google.com", "evil.com")
	if _, ok := f.Match("x.google.com"); ok {
		t.Error("feed entry under a trusted domain matched")
	}
	if _, ok := f.Match("a.evil.com"); !ok {
		t.Error("evil.com subdomain not matched")
	}
}

func TestBareDomainsAreScreened(t *testing.T) {
	if got := bareRegex.FindAllString("go to evil.com/login now", -1); len(got) != 1 || got[0] != "evil.com/login" {
		t.Fatalf("bare = %q", got)
	}
}

func TestQuoteStripsControlAndBidi(t *testing.T) {
	if got := Quote("a‮b`c\x00"); got != "abˋc" {
		t.Fatalf("Quote = %q", got)
	}
}

func TestSplitMessageKeepsFencesBalanced(t *testing.T) {
	msg := "```" + strings.Repeat("a", 3000) + "```"
	for i, c := range splitMessage(msg) {
		if strings.Count(c, "```")%2 != 0 || len(c) > discordMessageLimit {
			t.Fatalf("chunk %d unbalanced or too long (%d)", i, len(c))
		}
	}
}

// Text the bot posts for a member is checked with the same lists as a message.
func TestFlagFindsBlacklistedAndPhishingLinks(t *testing.T) {
	puny := "discord-gift.example"
	l := NewLinkChecker([]string{"free-nitro"}, feedWith(t, puny), nil, nil)

	for text, want := range map[string]string{
		"Θέματα https://free-nitro.example/claim": "free-nitro",
		"σημειώσεις www.free-nitro.org":           "free-nitro",
		"κέρδισε στο https://" + puny + "/x":      puny,
	} {
		got, ok := l.Flag(text)
		if !ok || got != want {
			t.Errorf("Flag(%q) = %q, %v; want %q", text, got, ok, want)
		}
	}
	for _, text := range []string{"Σημειώσεις 3ου κεφαλαίου", "Θέματα Ιαν-Φεβ 2025", "e.g. main.go", ""} {
		if got, ok := l.Flag(text); ok {
			t.Errorf("Flag(%q) flagged %q", text, got)
		}
	}
}
