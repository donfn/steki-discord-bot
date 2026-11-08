package moderation

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestDupTrackerTripsOnThirdChannel(t *testing.T) {
	tr := newDupTracker()
	t0 := time.Now()
	post := func(ch, id string, d time.Duration) []dupPost {
		return tr.add("u", dupPost{key: "k", channelID: ch, messageID: id, at: t0.Add(d)})
	}

	if post("a", "1", 0) != nil || post("a", "2", time.Second) != nil || post("b", "3", 2*time.Second) != nil {
		t.Fatal("tripped before a third distinct channel")
	}
	copies := post("c", "4", 3*time.Second)
	if len(copies) != 4 {
		t.Fatalf("copies = %d, want all 4 (including the repeat in channel a)", len(copies))
	}
	// The burst is forgotten once reported.
	if post("d", "5", 4*time.Second) != nil {
		t.Fatal("same burst tripped twice")
	}
}

func TestDupTrackerWindow(t *testing.T) {
	tr := newDupTracker()
	t0 := time.Now()
	tr.add("u", dupPost{key: "k", channelID: "a", at: t0})
	tr.add("u", dupPost{key: "k", channelID: "b", at: t0.Add(30 * time.Second)})
	if tr.add("u", dupPost{key: "k", channelID: "c", at: t0.Add(61 * time.Second)}) != nil {
		t.Fatal("tripped with the first copy outside the window")
	}

	tr.sweep(t0.Add(3 * time.Minute))
	if len(tr.posts) != 0 {
		t.Fatal("sweep kept stale members")
	}
}

func TestDupTrackerIsPerUserAndPerContent(t *testing.T) {
	tr := newDupTracker()
	t0 := time.Now()
	for i, ch := range []string{"a", "b", "c"} {
		if tr.add(ch, dupPost{key: "k", channelID: ch, at: t0}) != nil {
			t.Fatal("different users tripped together")
		}
		if tr.add("u", dupPost{key: string(rune('x' + i)), channelID: ch, at: t0}) != nil {
			t.Fatal("different messages tripped together")
		}
	}
}

func TestDupKey(t *testing.T) {
	short := &discordgo.Message{Content: "οκ"}
	if dupKey(short) != "" {
		t.Error("short text was fingerprinted")
	}
	a := dupKey(&discordgo.Message{Content: "Free Nitro   for everyone here"})
	b := dupKey(&discordgo.Message{Content: "free nitro for everyone HERE"})
	if a == "" || a != b {
		t.Error("whitespace and case changed the fingerprint")
	}
	img := &discordgo.Message{Attachments: []*discordgo.MessageAttachment{{Filename: "x.png", Size: 10}}}
	if dupKey(img) == "" {
		t.Error("attachment-only message was not fingerprinted")
	}
}

func TestInviteRegex(t *testing.T) {
	cases := map[string]string{
		"join discord.gg/AbC123 now":             "AbC123",
		"https://discord.com/invite/steki":       "steki",
		"https://discordapp.com/invite/x-y":      "x-y",
		"vanity dsc.gg/scam":                     "scam",
		"no invite https://discord.com/channels": "",
		"https://ptb.discord.com/invite/sub":     "sub",
		"evil-discord.gg/trap":                   "",
		"notdiscord.gg/trap":                     "",
		"discord.gg.evil.com/trap":               "",
	}
	for in, want := range cases {
		m := inviteRegex.FindStringSubmatch(in)
		got := ""
		if m != nil {
			got = m[2]
		}
		if got != want {
			t.Errorf("%q: code = %q, want %q", in, got, want)
		}
	}
}
