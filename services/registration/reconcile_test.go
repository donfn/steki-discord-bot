package registration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/bot"
	"steki-bot/internal/store"
)

// The reconcile tests run the whole pass: a real database, the fake Discord,
// and the guild each test describes.

func TestReconcileLeavesAHealthyGuildAlone(t *testing.T) {
	hn := newHarness(t)
	ch := hn.fd.addChannel(bot.RegisterChannelName("100"))
	hn.fd.addMember("100", time.Now().Add(-time.Hour))
	hn.register("100", ch)
	hn.fd.addMember("200", time.Now().Add(-90*24*time.Hour), bot.HMURoleID) // long verified

	rep, err := hn.h.reconcile(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.changed() != 0 || rep.Waiting != 0 || rep.Members != 2 {
		t.Errorf("a healthy guild changed: %+v", rep)
	}
	if n := hn.fd.count("DELETE /channels/*") + hn.fd.count("POST /guilds/"+testGuild+"/channels"); n != 0 {
		t.Errorf("%d channels were made or deleted in a healthy guild", n)
	}
	if hn.h.lastReconcile.Load() == 0 {
		t.Error("the finished pass was not recorded for /bot stats")
	}
}

func TestReconcileClosesTheRegistrationOfAMemberWhoLeft(t *testing.T) {
	hn := newHarness(t)
	ch := hn.fd.addChannel(bot.RegisterChannelName("100"))
	hn.register("100", ch) // not in the member list: they left

	rep, err := hn.h.reconcile(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Closed != 1 {
		t.Fatalf("closed %d registrations, want 1: %+v", rep.Closed, rep)
	}
	if _, open := hn.open("100"); open {
		t.Error("the registration of a member who left is still open")
	}
	if hn.fd.count("DELETE /channels/"+ch) != 1 {
		t.Error("their register channel was not deleted")
	}
}

func TestReconcileClosesTheRegistrationOfAMemberWhoVerified(t *testing.T) {
	hn := newHarness(t)
	ch := hn.fd.addChannel(bot.RegisterChannelName("100"))
	hn.fd.addMember("100", time.Now().Add(-time.Hour), bot.HMURoleID)
	hn.register("100", ch)

	rep, err := hn.h.reconcile(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Closed != 1 {
		t.Fatalf("closed %d registrations, want 1: %+v", rep.Closed, rep)
	}
	if _, open := hn.open("100"); open {
		t.Error("a verified member still has an open registration")
	}
}

func TestReconcileOnboardsOnlyRecentJoinsTheBotMissed(t *testing.T) {
	hn := newHarness(t)
	hn.fd.addMember("100", time.Now().Add(-time.Hour))                     // joined while the bot was away
	hn.fd.addMember("200", time.Now().Add(-60*24*time.Hour))               // an old member with no role: not a new join
	hn.fd.addMember("300", time.Now().Add(-time.Hour), bot.StaffRoleID)    // staff are never registered
	hn.fd.addBot("400")                                                    // nor are bots
	hn.fd.addMember("500", time.Now().Add(-time.Hour), bot.OutsiderRoleID) // already verified

	rep, err := hn.h.reconcile(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Recreated != 1 {
		t.Fatalf("made %d channels, want 1: %+v", rep.Recreated, rep)
	}
	if got := hn.fd.created(); len(got) != 1 || got[0] != bot.RegisterChannelName("100") {
		t.Errorf("channels made = %v, want only %s", got, bot.RegisterChannelName("100"))
	}
	reg, open := hn.open("100")
	if !open || reg.ChannelID == nil {
		t.Fatalf("the missed join has no registration with a channel: %+v", reg)
	}
	for _, id := range []string{"200", "300", "400", "500"} {
		if _, open := hn.open(id); open {
			t.Errorf("member %s was given a registration", id)
		}
	}
}

func TestReconcileRecreatesAChannelThatWasDeleted(t *testing.T) {
	hn := newHarness(t)
	hn.fd.addMember("100", time.Now().Add(-2*time.Hour))
	hn.register("100", "999000000000000001") // a channel Discord no longer has

	rep, err := hn.h.reconcile(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Recreated != 1 {
		t.Fatalf("made %d channels, want 1: %+v", rep.Recreated, rep)
	}
	reg, open := hn.open("100")
	if !open || reg.ChannelID == nil || *reg.ChannelID == "999000000000000001" {
		t.Errorf("the registration still points at the lost channel: %+v", reg)
	}
}

func TestReconcileRemovesOrphanAndDuplicateChannels(t *testing.T) {
	hn := newHarness(t)
	hn.fd.addMember("100", time.Now().Add(-time.Hour))
	kept := hn.fd.addChannel(bot.RegisterChannelName("100"))
	hn.register("100", kept)
	duplicate := hn.fd.addChannel(bot.RegisterChannelName("100")) // a second one for the same member
	orphan := hn.fd.addChannel(bot.RegisterChannelName("777"))    // nobody of that name is here

	rep, err := hn.h.reconcile(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 1 || rep.Duplicates != 1 || rep.changed() != 2 {
		t.Fatalf("report = %+v, want one orphan and one duplicate", rep)
	}
	if hn.fd.count("DELETE /channels/"+orphan) != 1 || hn.fd.count("DELETE /channels/"+duplicate) != 1 {
		t.Error("the orphan or the duplicate was not deleted")
	}
	if hn.fd.count("DELETE /channels/"+kept) != 0 {
		t.Error("the channel the registration points at was deleted")
	}
}

// TestReconcileStopsAtItsLimitAndASecondRunFinishes covers a long outage: more
// joins were missed than one pass may answer, and running the command again
// takes the rest.
func TestReconcileStopsAtItsLimitAndASecondRunFinishes(t *testing.T) {
	hn := newHarness(t)
	const missed = maxOnboardPerSweep + 5
	for i := range missed {
		hn.fd.addMember(fmt.Sprint(1000+i), time.Now().Add(-time.Hour))
	}

	first, err := hn.h.reconcile(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if first.Recreated != maxOnboardPerSweep || first.Waiting != 5 {
		t.Fatalf("first run = %+v, want %d made and 5 waiting", first, maxOnboardPerSweep)
	}
	if !strings.Contains(first.summary(nil), "Τρέξε ξανά") {
		t.Error("the reply does not tell the moderator to run it again")
	}

	second, err := hn.h.reconcile(hn.s)
	if err != nil {
		t.Fatal(err)
	}
	if second.Recreated != 5 || second.Waiting != 0 {
		t.Fatalf("second run = %+v, want the remaining 5 made and none waiting", second)
	}
	if n := len(hn.fd.created()); n != missed {
		t.Errorf("%d channels made in all, want %d, one for each member", n, missed)
	}
}

func TestReconcileRefusesToRunTwiceAtOnce(t *testing.T) {
	hn := newHarness(t)
	end, ok := hn.h.Jobs.Begin("reconcile")
	if !ok {
		t.Fatal("could not start the job")
	}
	defer end()

	if _, err := hn.h.reconcile(hn.s); !errors.Is(err, errJobRunning) {
		t.Errorf("err = %v, want errJobRunning", err)
	}
	if len(hn.fd.calls) != 0 {
		t.Errorf("the refused pass still called Discord: %v", hn.fd.calls)
	}
}

func TestReconcileNeedsTheGuild(t *testing.T) {
	hn := newHarness(t)
	hn.h.SetGuild("")
	if _, err := hn.h.reconcile(hn.s); !errors.Is(err, bot.ErrGuildUnknown) {
		t.Errorf("err = %v, want bot.ErrGuildUnknown", err)
	}
}

func TestReconcileReportsWhyItStopped(t *testing.T) {
	hn := newHarness(t)
	hn.fd.status["GET /guilds/"+testGuild+"/members"] = 403

	rep, err := hn.h.reconcile(hn.s)
	if err == nil || !strings.Contains(err.Error(), "list members") {
		t.Fatalf("err = %v, want the member listing named", err)
	}
	if hn.h.lastReconcile.Load() != 0 {
		t.Error("a pass that failed was recorded as finished")
	}
	// A pass that failed must not leave the job marked as running.
	if _, err := hn.h.reconcile(hn.s); errors.Is(err, errJobRunning) {
		t.Error("the failed pass left the job running")
	}
	if got := rep.summary(err); strings.Contains(got, "✅") || !strings.Contains(got, "σταμάτησε") {
		t.Errorf("the reply claims success after a failure:\n%s", got)
	}
}

// commandReply runs a command handler and returns what it told the moderator.
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

func TestRegReconcileCommandAnswersAndLeavesATrail(t *testing.T) {
	hn := newHarness(t)
	hn.fd.addMember("100", time.Now().Add(-time.Hour))

	reply := commandReply(t, hn, func(i *discordgo.InteractionCreate) { hn.h.cmdRegReconcile(hn.s, i, nil) })
	if !strings.Contains(reply, "Ο συγχρονισμός τελείωσε") || !strings.Contains(reply, "Κανάλια εγγραφής που φτιάχτηκαν: 1") {
		t.Errorf("the reply does not say what was done:\n%s", reply)
	}

	// The moderator who ran it is on record, with what it did.
	events, err := hn.st.CountEventsByKind(context.Background(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if events[store.EventModAction] != 1 {
		t.Errorf("mod actions on record = %d, want 1 (%v)", events[store.EventModAction], events)
	}
	// Something changed, so the moderators' log hears of it.
	if hn.fd.count("POST /channels/*/messages") < 2 {
		t.Error("the change was not announced in the log channel")
	}

	// Asked again with nothing left to do, it says so and stays quiet in the log.
	posts := hn.fd.count("POST /channels/*/messages")
	reply = commandReply(t, hn, func(i *discordgo.InteractionCreate) { hn.h.cmdRegReconcile(hn.s, i, nil) })
	if !strings.Contains(reply, "ήδη σωστά") {
		t.Errorf("a second run should find everything in order:\n%s", reply)
	}
	if hn.fd.count("POST /channels/*/messages") != posts {
		t.Error("a pass that changed nothing still posted to the log channel")
	}
}

func TestRegReconcileCommandSaysWhenItIsAlreadyRunning(t *testing.T) {
	hn := newHarness(t)
	end, _ := hn.h.Jobs.Begin("reconcile")
	defer end()

	reply := commandReply(t, hn, func(i *discordgo.InteractionCreate) { hn.h.cmdRegReconcile(hn.s, i, nil) })
	if !strings.Contains(reply, "τρέχει ήδη") {
		t.Errorf("reply = %q, want it to say the job is running", reply)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting for the background work")
}
