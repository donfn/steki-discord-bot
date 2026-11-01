package registration

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestCleanEmailInput(t *testing.T) {
	for in, want := range map[string]string{
		"TH12345@EDU.HMU.GR":             "th12345@edu.hmu.gr",
		"  th12345@edu.hmu.gr \n":        "th12345@edu.hmu.gr",
		"th 12345 @ edu.hmu.gr":          "th12345@edu.hmu.gr",
		"th12345\u200b@edu.hmu.gr\ufeff": "th12345@edu.hmu.gr",
		"th12345\uff20edu.hmu.gr":        "th12345@edu.hmu.gr",
		"mailto:th12345@edu.hmu.gr":      "th12345@edu.hmu.gr",
		"<th12345@edu.hmu.gr>.":          "th12345@edu.hmu.gr",
		`"csd4567@csd.uoc.gr"`:           "csd4567@csd.uoc.gr",
	} {
		if got := cleanEmailInput(in); got != want {
			t.Errorf("cleanEmailInput(%q) = %q, want %q", in, got, want)
		}
		if _, ok := classifyEmail(cleanEmailInput(in)); !ok {
			t.Errorf("cleaned %q is not eligible", in)
		}
	}
}

func TestModalValue(t *testing.T) {
	rows := []discordgo.MessageComponent{&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		&discordgo.TextInput{CustomID: emailInputID, Value: "th1@edu.hmu.gr"},
	}}}
	if got := modalValue(rows, emailInputID); got != "th1@edu.hmu.gr" {
		t.Fatalf("modalValue = %q", got)
	}
	if got := modalValue(rows, "other"); got != "" {
		t.Fatalf("modalValue(other) = %q", got)
	}
}

// The member can read and react in their channel but never write in it.
func TestRegisterChannelIsReadOnlyForMember(t *testing.T) {
	data := registerChannelData("guild", "42")
	var member *discordgo.PermissionOverwrite
	for _, ow := range data.PermissionOverwrites {
		if ow.ID == "42" {
			member = ow
		}
	}
	if member == nil {
		t.Fatal("no overwrite for the member")
	}
	if member.Allow&discordgo.PermissionViewChannel == 0 || member.Allow&discordgo.PermissionAddReactions == 0 {
		t.Errorf("member cannot view or react: allow=%b", member.Allow)
	}
	if member.Deny&discordgo.PermissionSendMessages == 0 || member.Allow&discordgo.PermissionSendMessages != 0 {
		t.Errorf("member can still send messages: allow=%b deny=%b", member.Allow, member.Deny)
	}
}
