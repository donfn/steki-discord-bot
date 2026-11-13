package moderation

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

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

func TestFileKeyAndHash(t *testing.T) {
	a := &discordgo.MessageAttachment{Filename: "Image.JPG", Size: 123}
	if got := fileKey(a); got != "image.jpg:123" {
		t.Errorf("fileKey = %q", got)
	}
	if sha([]byte("scam")) != sha([]byte("scam")) || sha([]byte("scam")) == sha([]byte("other")) {
		t.Error("sha does not tell files apart")
	}
	if _, ok := readFile(strings.NewReader(strings.Repeat("x", intelMaxFile+1))); ok {
		t.Error("an oversized file was read")
	}
	if data, ok := readFile(strings.NewReader("ok")); !ok || string(data) != "ok" {
		t.Error("a small file was not read")
	}
}

func TestFileExt(t *testing.T) {
	for name, want := range map[string]string{"a.JPG": ".jpg", "a.png": ".png", "noext": ".png", "x.p/../ng": ".png", "a.verylongext": ".png"} {
		if got := fileExt(name); got != want {
			t.Errorf("fileExt(%q) = %q, want %q", name, got, want)
		}
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
	embed, components := intelCard(7, spamItem{kind: store.IntelMedia, key: "k", label: "a`b.png"}, "u1", true)
	if !strings.Contains(embed.Fields[0].Value, "aˋb.png") {
		t.Fatalf("card = %+v", embed)
	}
	if !strings.Contains(embed.Description, "<@u1>") {
		t.Error("card does not name the member")
	}
	if len(components) != 1 {
		t.Fatalf("components = %d", len(components))
	}
}
