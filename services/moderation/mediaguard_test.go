package moderation

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/golang-jwt/jwt/v5"

	"steki-bot/tokens"
)

func TestFlaggedPost(t *testing.T) {
	cases := []struct {
		name string
		msg  *discordgo.Message
		want bool
	}{
		{"plain text", &discordgo.Message{Content: "καλημέρα"}, false},
		{"link", &discordgo.Message{Content: "δες https://example.com/x"}, true},
		{"www link", &discordgo.Message{Content: "www.example.com"}, true},
		{"attachment", &discordgo.Message{Attachments: []*discordgo.MessageAttachment{{ID: "1"}}}, true},
	}
	for _, c := range cases {
		if got := flaggedPost(c.msg); got != c.want {
			t.Errorf("%s: flaggedPost = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGuardMessageButton(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		_, comps := guardMessage("", "https://x/verify?t=abc", disabled)
		btn := comps[0].(discordgo.ActionsRow).Components[0].(discordgo.Button)
		if btn.Label != "Αφαίρεση περιορισμών" || btn.Style != discordgo.LinkButton {
			t.Fatalf("button = %+v", btn)
		}
		if btn.Disabled != disabled {
			t.Fatalf("disabled = %v, want %v", btn.Disabled, disabled)
		}
	}
}

func TestRestrictionWordingPerReason(t *testing.T) {
	embed, _ := guardMessage(tokens.ReasonManual, "https://x", false)
	if !strings.Contains(embed.Description, "Ομάδα Διαχείρισης") {
		t.Error("manual DM does not say the moderators applied it")
	}
	if got := restrictionSubject("u", "c", tokens.ReasonManual, "mod"); !strings.Contains(got, "<@mod>") || !strings.Contains(got, "<@u>") {
		t.Errorf("manual mod-log subject = %q", got)
	}
	if got := restrictionSubject("u", "c", tokens.ReasonDuplicate, ""); !strings.Contains(got, "<#c>") {
		t.Errorf("duplicate mod-log subject = %q", got)
	}
}

func TestOwnTimeout(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	// The link's expiry is stored in whole seconds; the timeout is not.
	linkEnd := now.Add(guardTimeout).Truncate(time.Second)
	c := &tokens.Claims{RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(linkEnd)}}
	at := func(d time.Duration) *time.Time { t := linkEnd.Add(d); return &t }

	cases := []struct {
		name  string
		until *time.Time
		want  bool
	}{
		{"the bot's own timeout", at(400 * time.Millisecond), true},
		{"no timeout", nil, false},
		{"already over", at(-guardTimeout - time.Hour), false},
		{"a moderator's shorter timeout", at(-12 * time.Hour), false},
		{"a moderator's longer timeout", at(6 * 24 * time.Hour), false},
		{"replaced a minute later", at(time.Minute), false},
	}
	for _, tc := range cases {
		if got := ownTimeout(tc.until, c, now); got != tc.want {
			t.Errorf("%s: ownTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}
