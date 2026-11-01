package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func commandReply(t *testing.T, hn *harness, run func(*discordgo.InteractionCreate)) string {
	t.Helper()
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		AppID: "app", Token: "tok", GuildID: testGuild,
		Member: &discordgo.Member{User: &discordgo.User{ID: "mod1"}},
	}}
	before := len(hn.fd.bodies["PATCH /webhooks/app/tok/messages/@original"])
	run(i)
	got := hn.fd.bodies["PATCH /webhooks/app/tok/messages/@original"]
	if len(got) != before+1 {
		t.Fatalf("the command sent %d replies, want 1", len(got)-before)
	}
	var out struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(got[len(got)-1]), &out); err != nil {
		t.Fatal(err)
	}
	return out.Content
}

func TestMCPRefreshEditsOnlyTheAnnouncementsWithTheOldButton(t *testing.T) {
	hn := newHarness(t)
	hn.s.State.User = &discordgo.User{ID: "bot"}
	general := hn.fd.addChannel("general")
	hidden := hn.fd.addChannel("staff-only")
	hn.fd.status["GET /channels/"+hidden+"/messages"] = 403 // the bot cannot read it

	hn.fd.say(general, "m1", "bot", mcpAnnouncementTitle, true)    // old button: to be edited
	hn.fd.say(general, "m2", "bot", mcpAnnouncementTitle, false)   // already current
	hn.fd.say(general, "m3", "member", mcpAnnouncementTitle, true) // a member quoting it
	hn.fd.say(general, "m4", "bot", "something else", true)        // another embed of the bot

	edited, failed, err := hn.h.refreshMCPAnnouncements(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if edited != 1 || failed != 0 {
		t.Errorf("edited %d, failed %d; want 1 and 0", edited, failed)
	}
	if hn.fd.count("PATCH /channels/"+general+"/messages/m1") != 1 || hn.fd.count("PATCH /channels/*/messages/*") != 1 {
		t.Errorf("wrong messages edited: %v", hn.fd.calls)
	}
}

func TestOnDemandMaterialCommandsSayWhenTheyAreOff(t *testing.T) {
	hn := newHarness(t) // no bucket, so no material worker and no stickies
	for name, run := range map[string]func(*discordgo.InteractionCreate){
		"/materials stickies": func(i *discordgo.InteractionCreate) { hn.h.cmdMaterialsStickies(hn.s, i, nil) },
		"/materials cards":    func(i *discordgo.InteractionCreate) { hn.h.cmdMaterialsCards(hn.s, i, nil) },
		"/mcp refresh":        func(i *discordgo.InteractionCreate) { hn.h.cmdMCPRefresh(hn.s, i, nil) },
	} {
		reply := commandReply(t, hn, run)
		if !strings.Contains(reply, "απενεργοποιημέν") { // απενεργοποιημένο, -ος, -α
			t.Errorf("%s: reply = %q, want it to say the feature is off", name, reply)
		}
	}
	if hn.fd.count("GET *") != 0 {
		t.Errorf("a command that is off still read from Discord: %v", hn.fd.calls)
	}
}

// TestConnectingRepairsNothing is the other half of the rule that a reconcile
// is run by a moderator: the gateway connecting, reconnecting and resuming
// must not read the member list or touch any channel or message. Installing
// the slash commands is the one thing a connection does, once per process.
