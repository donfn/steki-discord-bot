package registration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
	"steki-bot/dc"
	"steki-bot/rpc"
	"steki-bot/store"
)

// startMaintenance launches the reaper, which enforces the abandonment policy
// (nudge at 1h, kick at 6h) and is what keeps the register categories from filling
// up again. It depends on the clock, so it cannot wait for a moderator.
//
// Nothing here repairs drift between Discord and the database. That is
// reconcile, which only runs when a moderator asks (/reg reconcile), as do the
// other catch-up passes (/materials stickies, /materials cards, /mcp refresh).
func (h *Handler) startMaintenance(s *discordgo.Session) {
	h.GoLoop("reaper", sweepEvery, func() { h.reapAbandoned(s) })
}

// reapAbandoned nudges and then removes members who stopped responding.
func (h *Handler) reapAbandoned(s *discordgo.Session) {
	ctx, cancel := h.CtxTimeout(bot.ReapTimeout)
	defer cancel()

	// Kicks first: a registration past six hours should not also receive a
	// nudge on the same tick.
	h.reapKicks(ctx, s)
	h.reapNudges(ctx, s)
}

func (h *Handler) reapNudges(ctx context.Context, s *discordgo.Session) {
	regs, err := h.st.ListNudgeCandidates(ctx, nudgeAfter, maxReapPerTick)
	if err != nil {
		h.Log.Error("list nudge candidates failed", "error", err)
		return
	}

	for _, reg := range regs {
		if reg.ChannelID == nil || reg.IdleFor(time.Now()) >= kickAfter {
			continue // the kick pass owns anything this stale
		}

		msg := h.Send(s, *reg.ChannelID, fmt.Sprintf(msgNudge, reg.UserID))
		if msg == nil {
			continue // the channel is gone; /reg reconcile sorts the row out
		}
		if err := h.st.MarkNudged(ctx, reg.ID); err != nil {
			h.Log.Error("mark nudged failed", "reg_id", reg.ID, "error", err)
			continue
		}
		h.Audit(reg.UserID, reg.ID, store.EventNudged,
			map[string]any{"idle_minutes": int(reg.IdleFor(time.Now()).Minutes())}, "")
		h.Log.Info("nudged idle registration", "user_id", reg.UserID, "reg_id", reg.ID)
	}
}

func (h *Handler) reapKicks(ctx context.Context, s *discordgo.Session) {
	guildID := h.GuildID()
	if guildID == "" {
		return
	}

	regs, err := h.st.ListKickCandidates(ctx, kickAfter, maxReapPerTick)
	if err != nil {
		h.Log.Error("list kick candidates failed", "error", err)
		return
	}
	for _, reg := range regs {
		h.kickAbandoned(ctx, s, guildID, reg)
	}
}

// kickAbandoned removes one member whose registration went cold, after
// checking every guard that could make a kick wrong.
func (h *Handler) kickAbandoned(ctx context.Context, s *discordgo.Session, guildID string, listed *store.Registration) {
	defer h.LockUser(listed.UserID)()

	// The row was listed before the lock was taken: the member may have
	// answered, finished or restarted since. Act only on what is true now.
	reg, err := h.st.OpenRegistration(ctx, listed.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		h.Log.Error("kick: reload registration failed", "user_id", listed.UserID, "error", err)
		return
	}
	if reg.ID != listed.ID || reg.IdleFor(time.Now()) < kickAfter {
		return
	}

	member, err := h.GuildMember(ctx, s, guildID, reg.UserID)
	if err != nil {
		if dc.IsMissingResource(err) {
			// Already gone: close the row and remove any stray channel.
			h.closeAndCleanup(ctx, s, reg, store.OutcomeLeft)
			return
		}
		h.Log.Error("kick: fetch member failed", "user_id", reg.UserID, "error", err)
		return
	}

	// Never kick staff, moderators, or anyone already verified. A verified
	// member with a stale open row is a bookkeeping problem, not an abandoned
	// signup.
	if bot.HasAnyRole(member, bot.ProtectedRoleIDs) {
		h.Log.Info("skipping kick for protected member", "user_id", reg.UserID)
		h.closeAndCleanup(ctx, s, reg, store.OutcomeVerified)
		return
	}
	if handled, err := h.restoreVerifiedRole(ctx, s, guildID, reg); handled || err != nil {
		if err != nil {
			h.Log.Error("kick: check verified status failed", "user_id", reg.UserID, "error", err)
		}
		return
	}

	if reg.ChannelID != nil {
		if err := h.DeleteChannel(ctx, s, *reg.ChannelID); err != nil {
			h.Log.Warn("kick: delete channel failed", "channel_id", *reg.ChannelID, "error", err)
		}
	}

	err = h.Retry(ctx, "member.kick", func() error {
		return s.GuildMemberDeleteWithReason(guildID, reg.UserID, msgKickReason, discordgo.WithContext(ctx))
	})
	if err != nil && !dc.IsMissingResource(err) {
		h.Log.Error("kick failed", "user_id", reg.UserID, "error", err)
		h.UrgentMods(s, "⚠️ Δεν μπόρεσα να αφαιρέσω τον <@%s> μετά από 6 ώρες αδράνειας: ```%v```",
			reg.UserID, err)
		return
	}

	if err := h.st.CloseRegistration(ctx, reg.ID, store.OutcomeAbandonedKicked); err != nil {
		h.Log.Error("close registration failed", "reg_id", reg.ID, "error", err)
	}
	if err := h.st.IncrementKicked(ctx, reg.UserID); err != nil {
		h.Log.Warn("increment kicked failed", "user_id", reg.UserID, "error", err)
	}
	idle := reg.IdleFor(time.Now())
	h.Audit(reg.UserID, reg.ID, store.EventKicked,
		map[string]any{"idle_hours": int(idle.Hours()), "step": reg.Step}, "")

	username := reg.UserID
	if member.User != nil {
		username = member.User.Username
	}
	h.Log.Info("kicked abandoned registration", "user_id", reg.UserID, "idle", idle.String())
	h.AlertMods(s, "🧹 Ο χρήστης **%s** (<@%s>) αφαιρέθηκε μετά από 6 ώρες χωρίς δραστηριότητα "+
		"(βήμα: `%s`). Μπορεί να ξαναμπεί όποτε θέλει.",
		username, reg.UserID, reg.Step)
}

// restoreVerifiedRole handles a member the database records as verified who
// holds no verified role — what a role grant that failed after the address was
// claimed leaves behind. Such a member is re-granted the role, never kicked.
// It reports whether it took the member over; an error means the verified
// status is unknown and the caller must not kick either.
func (h *Handler) restoreVerifiedRole(ctx context.Context, s *discordgo.Session,
	guildID string, reg *store.Registration) (bool, error) {

	rec, err := h.st.GetMember(ctx, reg.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if rec.VerifiedAt == nil || rec.VerifiedEmail == nil {
		return false, nil
	}

	email := *rec.VerifiedEmail
	roleID, ok := classifyEmail(email)
	if ok {
		err = h.GrantRole(ctx, s, guildID, reg.UserID, roleID)
	}
	if !ok || err != nil {
		// Restart the idle clock so this alert repeats every six hours, not
		// every sweep.
		if terr := h.st.Touch(ctx, reg.ID); terr != nil {
			h.Log.Warn("touch registration failed", "reg_id", reg.ID, "error", terr)
		}
		h.UrgentMods(s, "⚠️ <@&%s> Ο <@%s> είναι επαληθευμένος στη βάση (`%s`) αλλά δεν έχει ρόλο "+
			"και δεν μπόρεσα να του τον δώσω, οπότε δεν τον αφαίρεσα. %v",
			bot.StaffRoleID, reg.UserID, email, err)
		return true, nil
	}

	h.closeAndCleanup(ctx, s, reg, store.OutcomeVerified)
	h.Audit(reg.UserID, reg.ID, store.EventModAction,
		map[string]any{"action": "role_restored", "email": email}, "")
	h.AlertMods(s, "🛠️ Ο <@%s> ήταν επαληθευμένος στη βάση αλλά χωρίς ρόλο. Ο ρόλος αποκαταστάθηκε.", reg.UserID)
	return true, nil
}

// closeAndCleanup closes a registration row and removes its channel. The
// caller must hold the member's lock.
func (h *Handler) closeAndCleanup(ctx context.Context, s *discordgo.Session,
	reg *store.Registration, outcome string) {

	h.revokeHelp(ctx, s, reg.UserID)
	if reg.ChannelID != nil {
		if err := h.DeleteChannel(ctx, s, *reg.ChannelID); err != nil {
			h.Log.Warn("cleanup: delete channel failed", "channel_id", *reg.ChannelID, "error", err)
		}
	}
	if err := h.st.CloseRegistration(ctx, reg.ID, outcome); err != nil {
		h.Log.Error("cleanup: close registration failed", "reg_id", reg.ID, "error", err)
	}
}

// reconcileReport counts what one reconcile pass found and did.
type reconcileReport struct {
	Members    int // members of the guild
	Closed     int // open registrations closed: the member left or is verified
	Recreated  int // register channels made: one was lost, or a join was missed
	Orphans    int // register channels deleted that belonged to nobody
	Duplicates int // second channels of one member deleted
	Adopted    int // register channels given back to the registration that lost them
	Waiting    int // members left without a channel when the pass hit its limit
}

// changed is how many things the pass put right.
func (r reconcileReport) changed() int {
	return r.Closed + r.Recreated + r.Orphans + r.Duplicates + r.Adopted
}

var (
	errJobRunning = errors.New("the job is already running")
)

// reconcile repairs drift in both directions between Discord and the database:
// a join the gateway dropped while the bot was away, a member who left, a
// register channel deleted by hand. It never runs by itself; a moderator asks
// for it with /reg reconcile. Passes never overlap: errJobRunning is returned
// for one asked while another runs. On an error the report still counts what
// was done before it.
func (h *Handler) reconcile(s *discordgo.Session) (reconcileReport, error) {
	guildID := h.GuildID()
	if guildID == "" {
		return reconcileReport{}, bot.ErrGuildUnknown
	}
	end, ok := h.Jobs.Begin("reconcile")
	if !ok {
		return reconcileReport{}, errJobRunning
	}
	defer end()

	ctx, cancel := h.CtxTimeout(bot.ReconcileTimeout)
	defer cancel()

	// Snapshot the rows before the members. Every listed row then predates
	// the member list, so "not in the guild" really means the member left,
	// not that they joined while the list was being fetched.
	listedAt := time.Now()
	open, err := h.st.ListOpen(ctx)
	if err != nil {
		return reconcileReport{}, fmt.Errorf("list open registrations: %w", err)
	}
	members, err := h.AllMembers(ctx, s, guildID)
	if err != nil {
		return reconcileReport{}, fmt.Errorf("list members: %w", err)
	}

	p := &reconcilePass{
		h: h, s: s, ctx: ctx, guildID: guildID, listedAt: listedAt,
		members: make(map[string]*discordgo.Member, len(members)),
		openBy:  make(map[string]*store.Registration, len(open)),
		budget:  maxOnboardPerSweep,
	}
	for _, m := range members {
		if m.User != nil {
			p.members[m.User.ID] = m
		}
	}
	p.rep.Members = len(p.members)
	// The moderation service's member index gets the same refresh from it.
	if err := h.moderation.Call(ctx, rpc.PathRosterSync, rpc.None{}, nil); err != nil {
		h.Log.Warn("reconcile: ask moderation to refresh its member index", "error", err)
	}

	p.closeStale(open)
	if err := p.sweepChannels(); err != nil {
		return p.rep, err
	}
	p.onboardMissed()
	h.sweepHelp(ctx, s)

	h.lastReconcile.Store(time.Now().Unix())
	rep := p.rep
	h.Log.Info("reconcile finished",
		"members", rep.Members, "closed", rep.Closed, "recreated", rep.Recreated,
		"orphan_channels_deleted", rep.Orphans, "duplicate_channels_deleted", rep.Duplicates,
		"channels_adopted", rep.Adopted, "waiting", rep.Waiting)
	return rep, nil
}

// reconcilePass is what the steps of one reconcile share: the guild as the
// pass found it, and how many more channels it may make.
type reconcilePass struct {
	h        *Handler
	s        *discordgo.Session
	ctx      context.Context
	guildID  string
	listedAt time.Time
	members  map[string]*discordgo.Member   // the guild's members, by ID
	openBy   map[string]*store.Registration // open registrations, by member ID
	budget   int
	rep      reconcileReport
}

// makeChannel gives a member a register channel, or counts them as waiting when
// the pass has made as many as it may. why is for the log.
func (p *reconcilePass) makeChannel(user *discordgo.User, why string) {
	if p.budget == 0 {
		p.rep.Waiting++
		return
	}
	p.budget--
	p.h.Log.Info("reconcile: making a register channel", "user_id", user.ID, "why", why)
	if p.h.ensureRegisterChannel(p.s, p.guildID, user) {
		p.rep.Recreated++
	}
}

// closeStale goes through the open registrations. The ones whose member left or
// already verified are closed; the ones whose channel is gone get a new one.
func (p *reconcilePass) closeStale(open []*store.Registration) {
	for _, reg := range open {
		p.openBy[reg.UserID] = reg

		member, present := p.members[reg.UserID]
		switch {
		case !present:
			p.h.closeLocked(p.ctx, p.s, reg, store.OutcomeLeft)
			p.rep.Closed++
		case bot.HasAnyRole(member, bot.VerifiedRoleIDs):
			p.h.closeLocked(p.ctx, p.s, reg, store.OutcomeVerified)
			p.rep.Closed++
		case reg.ChannelID == nil:
			p.makeChannel(member.User, "the registration has no channel")
		default:
			exists, err := p.h.ChannelExists(p.ctx, p.s, *reg.ChannelID)
			switch {
			case err != nil:
				p.h.Log.Warn("reconcile: cannot check channel", "channel_id", *reg.ChannelID, "error", err)
			case !exists:
				// Still here, unverified, and without a channel: make one
				// rather than leave them stranded.
				p.makeChannel(member.User, "the register channel is gone")
			}
		}
	}
}

// sweepChannels goes through the register channels in the guild. The one each
// open registration points at stays; one nobody owns, or a second one for a
// member, is deleted; one whose registration lost track of it is given back.
func (p *reconcilePass) sweepChannels() error {
	channels, err := dc.GuildChannels(p.ctx, p.s, p.h.Log, p.guildID)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}
	live := make(map[string]bool, len(channels))
	for _, ch := range channels {
		live[ch.ID] = true
	}
	for _, ch := range channels {
		userID, ok := bot.UserIDFromChannelName(ch.Name)
		if !ok || ch.Type != discordgo.ChannelTypeGuildText || createdAfter(ch.ID, p.listedAt) {
			// A channel newer than the row snapshot belongs to a join this
			// pass cannot see yet; it is not an orphan.
			continue
		}

		switch fate := registerChannelFate(ch.ID, p.openBy[userID], p.members[userID], live); fate {
		case fateKeep:
		case fateDelete, fateDeleteDuplicate:
			if err := p.h.DeleteChannel(p.ctx, p.s, ch.ID); err != nil {
				p.h.Log.Warn("reconcile: delete register channel failed", "channel_id", ch.ID, "error", err)
				continue
			}
			if fate == fateDeleteDuplicate {
				p.rep.Duplicates++
			} else {
				p.rep.Orphans++
			}
		case fateAdopt:
			unlock := p.h.LockUser(userID)
			_, err := p.h.adoptChannel(p.ctx, p.guildID, userID, ch.ID)
			unlock()
			if err != nil {
				p.h.Log.Warn("reconcile: adopt channel failed", "channel_id", ch.ID, "error", err)
				continue
			}
			p.openBy[userID] = &store.Registration{UserID: userID, ChannelID: &ch.ID}
			p.rep.Adopted++
		}
	}
	return nil
}

// onboardMissed gives a channel to each recent joiner who has no registration:
// the joins the gateway dropped while the bot was away.
func (p *reconcilePass) onboardMissed() {
	cutoff := time.Now().Add(-missedJoinWindow)
	for id, member := range p.members {
		if member.User == nil || member.User.Bot || member.JoinedAt.Before(cutoff) {
			continue
		}
		if _, has := p.openBy[id]; has || bot.HasAnyRole(member, bot.ProtectedRoleIDs) {
			continue
		}
		p.makeChannel(member.User, "the member joined while the bot was away")
	}
}

// summary is the reply to the moderator who asked for the pass. err is why it
// stopped early, if it did.
func (r reconcileReport) summary(err error) string {
	var b strings.Builder
	if err != nil {
		fmt.Fprintf(&b, "⚠️ Ο συγχρονισμός σταμάτησε στη μέση: %v\n", err)
	} else {
		fmt.Fprintf(&b, "✅ Ο συγχρονισμός τελείωσε (%d μέλη στον server).\n", r.Members)
	}
	if r.changed() == 0 && r.Waiting == 0 {
		if err == nil {
			b.WriteString("Όλα ήταν ήδη σωστά, δεν χρειάστηκε καμία αλλαγή.")
		} else {
			b.WriteString("Μέχρι εκεί δεν άλλαξε τίποτα.")
		}
		return b.String()
	}
	line := func(n int, text string) {
		if n > 0 {
			fmt.Fprintf(&b, "• %s: %d\n", text, n)
		}
	}
	line(r.Closed, "Εγγραφές που έκλεισαν (το μέλος έφυγε ή επαληθεύτηκε)")
	line(r.Recreated, "Κανάλια εγγραφής που φτιάχτηκαν")
	line(r.Adopted, "Κανάλια που ξαναδέθηκαν με την εγγραφή τους")
	line(r.Orphans, "Κανάλια χωρίς εγγραφή που σβήστηκαν")
	line(r.Duplicates, "Διπλά κανάλια που σβήστηκαν")
	if r.Waiting > 0 {
		fmt.Fprintf(&b, "⚠️ %d μέλη περιμένουν κανάλι: κάθε εκτέλεση φτιάχνει το πολύ %d. Τρέξε ξανά την εντολή.",
			r.Waiting, maxOnboardPerSweep)
	}
	return strings.TrimRight(b.String(), "\n")
}

// channelFate is what reconcile does with one register channel.
type channelFate int

const (
	fateKeep            channelFate = iota // the member's open registration points at it
	fateDelete                             // the member left or is already verified
	fateDeleteDuplicate                    // the member's registration already has another live channel
	fateAdopt                              // the member is here, unverified, and has no live channel
)

// registerChannelFate decides what to do with a register channel, given the
// owner's open registration and guild membership (nil when absent) and the
// set of channels that currently exist.
//
// A member can own two register channels — the old bot made one per join, so
// a rejoin left a second. Only the one their registration points at is kept;
// adopting the other would move the registration back and forth between the
// two on every pass, resetting the member's idle clock each time.
func registerChannelFate(channelID string, reg *store.Registration, member *discordgo.Member,
	live map[string]bool) channelFate {

	hasChannel := reg != nil && reg.ChannelID != nil
	switch {
	case hasChannel && *reg.ChannelID == channelID:
		return fateKeep
	case member == nil || bot.HasAnyRole(member, bot.VerifiedRoleIDs):
		return fateDelete
	case hasChannel && live[*reg.ChannelID]:
		return fateDeleteDuplicate
	default:
		return fateAdopt
	}
}

// createdAfter reports whether a Discord object was created after t, judging by
// the timestamp embedded in its snowflake ID. Unparseable IDs count as new, so
// the caller errs on the side of leaving them alone.
func createdAfter(id string, t time.Time) bool {
	at, err := discordgo.SnowflakeTimestamp(id)
	return err != nil || at.After(t)
}

// closeLocked is closeAndCleanup for callers that do not hold the member's
// lock yet.
func (h *Handler) closeLocked(ctx context.Context, s *discordgo.Session, reg *store.Registration, outcome string) {
	defer h.LockUser(reg.UserID)()
	h.closeAndCleanup(ctx, s, reg, outcome)
}
