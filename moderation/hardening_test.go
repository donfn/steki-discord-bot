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
