package moderation

import (
	"strings"
	"testing"
	"time"

	"steki-bot/internal/store"
)

func TestLinkKey(t *testing.T) {
	same := []string{
		"https://Evil.test/gift/", "http://www.evil.test/gift", "evil.test/gift)", "https://evil.test/gift#top",
	}
	want, ok := linkKey(same[0], "steki.test")
	if !ok || want != "evil.test/gift" {
		t.Fatalf("key = %q, %v", want, ok)
	}
	for _, raw := range same[1:] {
		if got, _ := linkKey(raw, "steki.test"); got != want {
			t.Errorf("linkKey(%q) = %q, want %q", raw, got, want)
		}
	}
	if got, _ := linkKey("https://evil.test/gift?ref=1", ""); got == want {
		t.Error("a different query is the same link")
	}
	if got, _ := linkKey("https://evil.test/Gift", ""); got == want {
		t.Error("paths are case-sensitive")
	}
	for _, raw := range []string{
		"https://www.youtube.com/watch?v=x", "https://discord.com/channels/1/2", "https://cdn.discordapp.com/attachments/1/2/a.png",
		"https://steki.test/materials", "https://sub.github.com/x",
	} {
		if _, ok := linkKey(raw, "steki.test"); ok {
			t.Errorf("%q was learnable", raw)
		}
	}
}

func TestFromDiscordCDN(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://cdn.discordapp.com/attachments/1/2/a.png?ex=1": true,
		"https://media.discordapp.net/attachments/1/2/a.png":    true,
		"http://cdn.discordapp.com/a.png":                       false,
		"https://cdn.discordapp.com.evil.test/a.png":            false,
		"https://evil.test/cdn.discordapp.com/a.png":            false,
		"http://169.254.169.254/latest":                         false,
	} {
		if got := fromDiscordCDN(raw); got != want {
			t.Errorf("fromDiscordCDN(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestHashBody(t *testing.T) {
	a, ok := hashBody(strings.NewReader("scam.png"))
	b, _ := hashBody(strings.NewReader("scam.png"))
	c, _ := hashBody(strings.NewReader("other"))
	if !ok || a != b || a == c {
		t.Fatalf("hashes: %q %q %q", a, b, c)
	}
	if _, ok := hashBody(strings.NewReader(strings.Repeat("x", intelMaxFile+1))); ok {
		t.Error("an oversized file was hashed")
	}
}

func TestRepeatTrackerTripsOnSecondMessage(t *testing.T) {
	tr := newRepeatTracker()
	t0 := time.Now()
	post := func(key, ch, id string, d time.Duration) []dupPost {
		return tr.add("u", dupPost{key: key, channelID: ch, messageID: id, at: t0.Add(d)})
	}

	if post("media:a", "c1", "1", 0) != nil {
		t.Fatal("tripped on the first copy")
	}
	if post("link:b", "c1", "2", time.Second) != nil {
		t.Fatal("different items tripped together")
	}
	// The same channel counts: spam is often one channel, over and over.
	if got := post("media:a", "c1", "3", 2*time.Second); len(got) != 2 {
		t.Fatalf("copies = %d, want both messages", len(got))
	}
	// Two items of one message are one message.
	if post("media:c", "c1", "4", 3*time.Second) != nil || post("media:c", "c1", "4", 3*time.Second) != nil {
		t.Fatal("one message counted twice")
	}
	if post("media:a", "c2", "5", 6*time.Minute) != nil {
		t.Fatal("tripped with the first copy outside the window")
	}
}

func TestIntelCard(t *testing.T) {
	embed, components := intelCard(7, spamItem{kind: store.IntelMedia, key: "k", label: "a`b.png", image: "https://cdn.discordapp.com/x.png"}, "u1")
	if embed.Image == nil || !strings.Contains(embed.Fields[0].Value, "aˋb.png") {
		t.Fatalf("card = %+v", embed)
	}
	if !strings.Contains(embed.Description, "<@u1>") {
		t.Error("card does not name the member")
	}
	if len(components) != 1 {
		t.Fatalf("components = %d", len(components))
	}
}
