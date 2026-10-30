package events

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestIsOldMCPAnnouncement(t *testing.T) {
	bot := &discordgo.User{ID: "bot"}
	embeds, current := mcpAnnouncementCard()
	old := []discordgo.MessageComponent{&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		&discordgo.Button{Label: "Οδηγίες σύνδεσης", Style: discordgo.LinkButton, URL: "https://x/mcp/setup?t=a"},
	}}}
	now := []discordgo.MessageComponent{&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		&discordgo.Button{Label: "Οδηγίες σύνδεσης", Style: discordgo.PrimaryButton, CustomID: mcpGuideButtonID},
	}}}
	if len(current) != 1 || embeds[0].Title != mcpAnnouncementTitle {
		t.Fatal("unexpected card")
	}
	for name, tc := range map[string]struct {
		m    *discordgo.Message
		want bool
	}{
		"old":          {&discordgo.Message{Author: bot, Embeds: embeds, Components: old}, true},
		"current":      {&discordgo.Message{Author: bot, Embeds: embeds, Components: now}, false},
		"someone else": {&discordgo.Message{Author: &discordgo.User{ID: "u"}, Embeds: embeds, Components: old}, false},
		"other embed":  {&discordgo.Message{Author: bot, Embeds: []*discordgo.MessageEmbed{{Title: "x"}}, Components: old}, false},
	} {
		if got := isOldMCPAnnouncement(tc.m, "bot"); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
