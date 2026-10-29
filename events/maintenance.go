package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/store"
)

// startMaintenance launches the two background sweeps.
//
//   - the reaper enforces the abandonment policy (nudge at 1h, kick at 6h) and
//     is what keeps the register categories from filling up again;
//   - the reconciler repairs drift between Discord and the database, which is
//     how a join that arrived while the gateway was disconnected still ends up
//     with a channel.
func (h *Handler) startMaintenance(s *discordgo.Session) {
	h.goLoop("reaper", sweepEvery, func() { h.reapAbandoned(s) })
	h.goLoop("reconciler", reconcileEvery, func() { h.reconcile(s) })
}

// reapAbandoned nudges and then removes members who stopped responding.
func (h *Handler) reapAbandoned(s *discordgo.Session) {
	ctx, cancel := h.ctxTimeout(reapTimeout)
	defer cancel()

	// Kicks first: a registration past six hours should not also receive a
	// nudge on the same tick.
	h.reapKicks(ctx, s)
	h.reapNudges(ctx, s)
}

func (h *Handler) reapNudges(ctx context.Context, s *discordgo.Session) {
	regs, err := h.st.ListNudgeCandidates(ctx, nudgeAfter, maxReapPerTick)
	if err != nil {
		h.log.Error("list nudge candidates failed", "error", err)
		return
	}

	for _, reg := range regs {
		if reg.ChannelID == nil || reg.IdleFor(time.Now()) >= kickAfter {
			continue // the kick pass owns anything this stale
		}

		msg := h.send(s, *reg.ChannelID, fmt.Sprintf(msgNudge, reg.UserID))
		if msg == nil {
			continue // the channel is gone; the reconciler sorts the row out
		}
		if err := h.st.MarkNudged(ctx, reg.ID); err != nil {
			h.log.Error("mark nudged failed", "reg_id", reg.ID, "error", err)
			continue
		}
		h.audit(reg.UserID, reg.ID, store.EventNudged,
			map[string]any{"idle_minutes": int(reg.IdleFor(time.Now()).Minutes())}, "")
		h.log.Info("nudged idle registration", "user_id", reg.UserID, "reg_id", reg.ID)
	}
}

func (h *Handler) reapKicks(ctx context.Context, s *discordgo.Session) {
	guildID := h.guildID()
	if guildID == "" {
		return
	}

	regs, err := h.st.ListKickCandidates(ctx, kickAfter, maxReapPerTick)
	if err != nil {
		h.log.Error("list kick candidates failed", "error", err)
		return
	}
	for _, reg := range regs {
		h.kickAbandoned(ctx, s, guildID, reg)
	}
}

// kickAbandoned removes one member whose registration went cold, after
// checking every guard that could make a kick wrong.
func (h *Handler) kickAbandoned(ctx context.Context, s *discordgo.Session, guildID string, listed *store.Registration) {
	defer h.lockUser(listed.UserID)()

	// The row was listed before the lock was taken: the member may have
	// answered, finished or restarted since. Act only on what is true now.
	reg, err := h.st.OpenRegistration(ctx, listed.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		h.log.Error("kick: reload registration failed", "user_id", listed.UserID, "error", err)
		return
	}
	if reg.ID != listed.ID || reg.IdleFor(time.Now()) < kickAfter {
		return
	}

	member, err := h.guildMember(ctx, s, guildID, reg.UserID)
	if err != nil {
		if dc.IsMissingResource(err) {
			// Already gone: close the row and remove any stray channel.
			h.closeAndCleanup(ctx, s, reg, store.OutcomeLeft)
			return
		}
		h.log.Error("kick: fetch member failed", "user_id", reg.UserID, "error", err)
		return
	}

	// Never kick staff, moderators, or anyone already verified. A verified
	// member with a stale open row is a bookkeeping problem, not an abandoned
	// signup.
	if hasAnyRole(member, protectedRoleIDs) {
		h.log.Info("skipping kick for protected member", "user_id", reg.UserID)
		h.closeAndCleanup(ctx, s, reg, store.OutcomeVerified)
		return
	}
	if handled, err := h.restoreVerifiedRole(ctx, s, guildID, reg); handled || err != nil {
		if err != nil {
			h.log.Error("kick: check verified status failed", "user_id", reg.UserID, "error", err)
		}
		return
	}

	if reg.ChannelID != nil {
		if err := h.deleteChannel(ctx, s, *reg.ChannelID); err != nil {
			h.log.Warn("kick: delete channel failed", "channel_id", *reg.ChannelID, "error", err)
		}
	}

	err = h.retry(ctx, "member.kick", func() error {
		return s.GuildMemberDeleteWithReason(guildID, reg.UserID, msgKickReason, discordgo.WithContext(ctx))
	})
	if err != nil && !dc.IsMissingResource(err) {
		h.log.Error("kick failed", "user_id", reg.UserID, "error", err)
		h.alertMods(s, "⚠️ Δεν μπόρεσα να αφαιρέσω τον <@%s> μετά από 6 ώρες αδράνειας: ```%v```",
			reg.UserID, err)
		return
	}

	if err := h.st.CloseRegistration(ctx, reg.ID, store.OutcomeAbandonedKicked); err != nil {
		h.log.Error("close registration failed", "reg_id", reg.ID, "error", err)
	}
	if err := h.st.IncrementKicked(ctx, reg.UserID); err != nil {
		h.log.Warn("increment kicked failed", "user_id", reg.UserID, "error", err)
	}
	idle := reg.IdleFor(time.Now())
	h.audit(reg.UserID, reg.ID, store.EventKicked,
		map[string]any{"idle_hours": int(idle.Hours()), "step": reg.Step}, "")

	username := reg.UserID
	if member.User != nil {
		username = member.User.Username
	}
	h.log.Info("kicked abandoned registration", "user_id", reg.UserID, "idle", idle.String())
	h.alertMods(s, "🧹 Ο χρήστης **%s** (<@%s>) αφαιρέθηκε μετά από 6 ώρες χωρίς δραστηριότητα "+
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
		err = h.grantRole(ctx, s, guildID, reg.UserID, roleID)
	}
	if !ok || err != nil {
		// Restart the idle clock so this alert repeats every six hours, not
		// every sweep.
		if terr := h.st.Touch(ctx, reg.ID); terr != nil {
			h.log.Warn("touch registration failed", "reg_id", reg.ID, "error", terr)
		}
		h.alertMods(s, "⚠️ <@&%s> Ο <@%s> είναι επαληθευμένος στη βάση (`%s`) αλλά δεν έχει ρόλο "+
			"και δεν μπόρεσα να του τον δώσω. Δεν αφαιρέθηκε. %v",
			staffRoleID, reg.UserID, email, err)
		return true, nil
	}

	h.closeAndCleanup(ctx, s, reg, store.OutcomeVerified)
	h.audit(reg.UserID, reg.ID, store.EventModAction,
		map[string]any{"action": "role_restored", "email": email}, "")
	h.alertMods(s, "🛠️ Ο <@%s> ήταν επαληθευμένος στη βάση αλλά χωρίς ρόλο. Ο ρόλος αποκαταστάθηκε.", reg.UserID)
	return true, nil
}

// closeAndCleanup closes a registration row and removes its channel. The
// caller must hold the member's lock.
func (h *Handler) closeAndCleanup(ctx context.Context, s *discordgo.Session,
	reg *store.Registration, outcome string) {

	h.revokeHelp(ctx, s, reg.UserID)
	if reg.ChannelID != nil {
		if err := h.deleteChannel(ctx, s, *reg.ChannelID); err != nil {
			h.log.Warn("cleanup: delete channel failed", "channel_id", *reg.ChannelID, "error", err)
		}
	}
	if err := h.st.CloseRegistration(ctx, reg.ID, outcome); err != nil {
		h.log.Error("cleanup: close registration failed", "reg_id", reg.ID, "error", err)
	}
}

// reconcile repairs drift in both directions between Discord and the database.
// Passes never overlap: one triggered while another runs is skipped.
func (h *Handler) reconcile(s *discordgo.Session) {
	guildID := h.guildID()
	if guildID == "" {
		h.log.Debug("reconcile skipped: guild not known yet")
		return
	}
	if !h.reconciling.CompareAndSwap(false, true) {
		h.log.Debug("reconcile skipped: a pass is already running")
		return
	}
	defer h.reconciling.Store(false)

	ctx, cancel := h.ctxTimeout(reconcileTimeout)
	defer cancel()

	// Snapshot the rows before the members. Every listed row then predates
	// the member list, so "not in the guild" really means the member left,
	// not that they joined while the list was being fetched.
	listedAt := time.Now()
	open, err := h.st.ListOpen(ctx)
	if err != nil {
		h.log.Error("reconcile: list open failed", "error", err)
		return
	}

	members, err := h.allMembers(ctx, s, guildID)
	if err != nil {
		h.log.Error("reconcile: list members failed", "error", err)
		return
	}
	byID := make(map[string]*discordgo.Member, len(members))
	for _, m := range members {
		if m.User != nil {
			byID[m.User.ID] = m
		}
	}

	var closed, recreated, deleted, duplicates, adopted int
	budget := maxOnboardPerSweep

	// 1. Open rows whose member or channel no longer makes sense.
	openByUser := make(map[string]*store.Registration, len(open))

	for _, reg := range open {
		openByUser[reg.UserID] = reg

		member, present := byID[reg.UserID]
		switch {
		case !present:
			h.closeLocked(ctx, s, reg, store.OutcomeLeft)
			closed++
		case hasAnyRole(member, verifiedRoleIDs):
			h.closeLocked(ctx, s, reg, store.OutcomeVerified)
			closed++
		case reg.ChannelID == nil:
			if budget > 0 {
				h.ensureRegisterChannel(s, guildID, member.User)
				budget--
				recreated++
			}
		default:
			exists, err := h.channelExists(ctx, s, *reg.ChannelID)
			if err != nil {
				h.log.Warn("reconcile: cannot check channel", "channel_id", *reg.ChannelID, "error", err)
				continue
			}
			if !exists && budget > 0 {
				// Still here, unverified, and without a channel: recreate
				// it rather than leave them stranded.
				h.ensureRegisterChannel(s, guildID, member.User)
				budget--
				recreated++
			}
		}
	}

	// 2. Register channels with no corresponding open row.
	channels, err := dc.GuildChannels(ctx, s, h.log, guildID)
	if err != nil {
		h.log.Error("reconcile: list channels failed", "error", err)
		return
	}
	live := make(map[string]bool, len(channels))
	for _, ch := range channels {
		live[ch.ID] = true
	}
	for _, ch := range channels {
		userID, ok := userIDFromChannelName(ch.Name)
		if !ok || ch.Type != discordgo.ChannelTypeGuildText || createdAfter(ch.ID, listedAt) {
			// A channel newer than the row snapshot belongs to a join this
			// pass cannot see yet; it is not an orphan.
			continue
		}

		switch fate := registerChannelFate(ch.ID, openByUser[userID], byID[userID], live); fate {
		case fateKeep:
		case fateDelete, fateDeleteDuplicate:
			if err := h.deleteChannel(ctx, s, ch.ID); err != nil {
				h.log.Warn("reconcile: delete register channel failed", "channel_id", ch.ID, "error", err)
				continue
			}
			if fate == fateDeleteDuplicate {
				duplicates++
			} else {
				deleted++
			}
		case fateAdopt:
			unlock := h.lockUser(userID)
			_, err := h.adoptChannel(ctx, guildID, userID, ch.ID)
			unlock()
			if err != nil {
				h.log.Warn("reconcile: adopt channel failed", "channel_id", ch.ID, "error", err)
				continue
			}
			openByUser[userID] = &store.Registration{UserID: userID, ChannelID: &ch.ID}
			adopted++
		}
	}

	// 3. Recent joins with neither a row nor a channel: the joins dropped
	//    while the gateway was disconnected.
	cutoff := time.Now().Add(-missedJoinWindow)
	for id, member := range byID {
		if budget == 0 {
			h.log.Warn("reconcile: onboarding budget exhausted; the rest wait for the next pass")
			break
		}
		if member.User == nil || member.User.Bot || member.JoinedAt.Before(cutoff) {
			continue
		}
		if _, has := openByUser[id]; has || hasAnyRole(member, protectedRoleIDs) {
			continue
		}
		h.log.Info("reconcile: onboarding member missed at join time", "user_id", id)
		h.ensureRegisterChannel(s, guildID, member.User)
		budget--
		recreated++
	}

	h.sweepHelp(ctx, s)

	h.log.Info("reconcile finished",
		"members", len(byID), "closed", closed, "recreated", recreated,
		"orphan_channels_deleted", deleted, "duplicate_channels_deleted", duplicates,
		"channels_adopted", adopted)
}

// channelFate is what the reconciler does with one register channel.
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
	case member == nil || hasAnyRole(member, verifiedRoleIDs):
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
	defer h.lockUser(reg.UserID)()
	h.closeAndCleanup(ctx, s, reg, outcome)
}

// allMembers pages through the guild's member list.
func (h *Handler) allMembers(ctx context.Context, s *discordgo.Session, guildID string) ([]*discordgo.Member, error) {
	var all []*discordgo.Member
	after := ""

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var page []*discordgo.Member
		err := h.retry(ctx, "guild.members", func() error {
			p, perr := s.GuildMembers(guildID, after, 1000, discordgo.WithContext(ctx))
			if perr != nil {
				return perr
			}
			page = p
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		all = append(all, page...)

		last := page[len(page)-1]
		if last.User == nil || len(page) < 1000 {
			return all, nil
		}
		after = last.User.ID
	}
}
