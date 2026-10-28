package events

import (
	"testing"

	"github.com/bwmarrin/discordgo"
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
