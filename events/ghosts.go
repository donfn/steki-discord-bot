package events

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/store"
)

// The ghost role marks members who have not sent a message for three months.
// The member_activity table indexes every member of the guild with the time of
// their last message and whether they hold the role. A member with a message
// inside the window never has the role; one without has it. The clock of a
// member who has not posted yet starts at their join time, so a newcomer is
// not a ghost.

const (
	// ghostAfterMonths is how long a member may stay silent before they are a
	// ghost.
	ghostAfterMonths = 3

	// ghostScanTimeout bounds one history scan.
	ghostScanTimeout = 3 * time.Hour

	// ghostMaxConsecutiveFailures stops a role pass that is failing every call,
	// as it does when the role sits above the bot's own.
	ghostMaxConsecutiveFailures = 5
)

// ghostCutoff is the moment before which silence makes a ghost.
func ghostCutoff(now time.Time) time.Time {
	return now.AddDate(0, -ghostAfterMonths, 0)
}

// isGhost reports whether a member is a ghost: the later of their last message
// and their join time is older than the cutoff.
func isGhost(joined time.Time, lastMessage *time.Time, cutoff time.Time) bool {
	since := joined
	if lastMessage != nil && lastMessage.After(since) {
		since = *lastMessage
	}
	return since.Before(cutoff)
}

// countsAsActivity reports whether a message is one a member chose to send:
// not a bot's, a webhook's, or a system message such as a join notice, which
// carries the member as its author.
func countsAsActivity(m *discordgo.Message) bool {
	if m == nil || m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		return false
	}
	return m.Type == discordgo.MessageTypeDefault || m.Type == discordgo.MessageTypeReply
}

// joinedAt is when a member joined, or now when Discord did not say, which
// gives them the full window rather than ghosting them on missing data.
func joinedAt(m *discordgo.Member, now time.Time) time.Time {
	if m.JoinedAt.IsZero() {
		return now
	}
	return m.JoinedAt
}

// ghostReport counts what a reconciliation did, or would do on a dry run.
type ghostReport struct {
	members, ghosts, added, removed, failed int
}

// scanLastMessages reads every channel and thread the bot can see back to the
// cutoff and returns each author's latest message in that time, with how many
// channels it read.
func (h *Handler) scanLastMessages(ctx context.Context, s *discordgo.Session, guildID string, cutoff time.Time) (map[string]time.Time, int, error) {
	channels, err := dc.GuildChannels(ctx, s, h.log, guildID)
	if err != nil {
		return nil, 0, err
	}
	var targets []*discordgo.Channel
	for _, ch := range channels {
		switch ch.Type {
		case discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews,
			discordgo.ChannelTypeGuildVoice, discordgo.ChannelTypeGuildStageVoice:
			targets = append(targets, ch)
		}
	}
	targets = append(targets, h.materialThreads(s, guildID, channels)...)

	seen := map[string]time.Time{}
	for _, ch := range targets {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		err := h.channelActivity(ctx, s, ch.ID, cutoff, seen)
		switch {
		case dc.RESTCode(err) == 403 || dc.APICode(err) == discordgo.ErrCodeMissingAccess:
			h.log.Debug("ghosts: channel not readable; skipped", "channel_id", ch.ID, "name", ch.Name)
		case err != nil:
			h.log.Warn("ghosts: scan channel", "channel_id", ch.ID, "name", ch.Name, "error", err)
		}
	}
	return seen, len(targets), nil
}

// channelActivity pages back through one channel until the cutoff, noting each
// author's latest message in seen.
func (h *Handler) channelActivity(ctx context.Context, s *discordgo.Session, channelID string, cutoff time.Time, seen map[string]time.Time) error {
	before := ""
	for {
		var msgs []*discordgo.Message
		err := h.retry(ctx, "ghosts.history", func() error {
			var err error
			msgs, err = s.ChannelMessages(channelID, 100, before, "", "", discordgo.WithContext(ctx))
			return err
		})
		time.Sleep(scanPagePause)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Timestamp.Before(cutoff) {
				return nil
			}
			if countsAsActivity(m) && m.Timestamp.After(seen[m.Author.ID]) {
				seen[m.Author.ID] = m.Timestamp
			}
		}
		if len(msgs) < 100 {
			return nil
		}
		before = msgs[len(msgs)-1].ID
	}
}

// reconcileGhosts brings the index and the role in line with a member list and
// the messages a scan found. With dryRun it updates the index but leaves every
// role alone and reports what it would change.
func (h *Handler) reconcileGhosts(ctx context.Context, s *discordgo.Session, guildID string,
	members []*discordgo.Member, listedAt time.Time, seen map[string]time.Time, dryRun bool) (ghostReport, error) {

	now := time.Now()
	byID := make(map[string]*discordgo.Member, len(members))
	entries := make([]store.ActivityEntry, 0, len(members))
	for _, m := range members {
		if m.User == nil || m.User.Bot {
			continue
		}
		byID[m.User.ID] = m
		e := store.ActivityEntry{UserID: m.User.ID, JoinedAt: joinedAt(m, now)}
		if at, ok := seen[m.User.ID]; ok {
			e.LastMessageAt = &at
		}
		entries = append(entries, e)
	}
	if err := h.st.SyncMemberActivity(ctx, listedAt, entries); err != nil {
		return ghostReport{}, fmt.Errorf("sync activity index: %w", err)
	}
	rows, err := h.st.MemberActivities(ctx)
	if err != nil {
		return ghostReport{}, fmt.Errorf("read activity index: %w", err)
	}

	cutoff := ghostCutoff(now)
	rep := ghostReport{members: len(rows)}
	streak := 0
	for _, row := range rows {
		member := byID[row.UserID]
		if member == nil {
			continue
		}
		want := isGhost(row.JoinedAt, row.LastMessageAt, cutoff)
		has := slices.Contains(member.Roles, ghostRoleID)
		if want {
			rep.ghosts++
		}
		if want == has {
			if !dryRun && row.Ghost != want {
				_ = h.st.SetGhost(ctx, row.UserID, want)
			}
			continue
		}
		if dryRun {
			if want {
				rep.added++
			} else {
				rep.removed++
			}
			continue
		}
		if err := h.setGhostRole(ctx, s, guildID, row.UserID, want); err != nil {
			h.log.Warn("ghosts: change role", "user_id", row.UserID, "ghost", want, "error", err)
			rep.failed++
			if streak++; streak >= ghostMaxConsecutiveFailures {
				return rep, fmt.Errorf("stopped after %d failed role changes in a row: %w", streak, err)
			}
			continue
		}
		streak = 0
		if want {
			rep.added++
		} else {
			rep.removed++
		}
		if err := h.st.SetGhost(ctx, row.UserID, want); err != nil {
			h.log.Error("ghosts: record role", "user_id", row.UserID, "error", err)
		}
	}
	return rep, nil
}

// setGhostRole gives a member the ghost role or takes it away.
func (h *Handler) setGhostRole(ctx context.Context, s *discordgo.Session, guildID, userID string, add bool) error {
	return h.retry(ctx, "ghosts.role", func() error {
		if add {
			return s.GuildMemberRoleAdd(guildID, userID, ghostRoleID,
				discordgo.WithAuditLogReason("ghost: no message for 3 months"), discordgo.WithContext(ctx))
		}
		return s.GuildMemberRoleRemove(guildID, userID, ghostRoleID,
			discordgo.WithAuditLogReason("ghost: active again"), discordgo.WithContext(ctx))
	})
}

// runGhostScan is the whole pass: read the last three months of messages, fetch
// the member list, update the index and change the roles.
func (h *Handler) runGhostScan(s *discordgo.Session, dryRun bool) {
	guildID := h.guildID()
	if guildID == "" {
		return
	}
	ctx, cancel := h.ctxTimeout(ghostScanTimeout)
	defer cancel()

	started := time.Now()
	seen, channels, err := h.scanLastMessages(ctx, s, guildID, ghostCutoff(started))
	if err != nil {
		h.log.Error("ghosts: scan failed", "error", err)
		h.alertMods(s, "⚠️ Ο έλεγχος δραστηριότητας απέτυχε: ```%v```", err)
		return
	}
	listedAt := time.Now()
	members, err := h.allMembers(ctx, s, guildID)
	if err != nil {
		h.log.Error("ghosts: list members failed", "error", err)
		h.alertMods(s, "⚠️ Ο έλεγχος δραστηριότητας απέτυχε: ```%v```", err)
		return
	}
	rep, err := h.reconcileGhosts(ctx, s, guildID, members, listedAt, seen, dryRun)
	h.log.Info("ghosts: scan finished", "channels", channels, "members", rep.members, "ghosts", rep.ghosts,
		"added", rep.added, "removed", rep.removed, "failed", rep.failed, "dry_run", dryRun,
		"took", time.Since(started).Round(time.Second))

	verb := "προστέθηκε σε %d, αφαιρέθηκε από %d"
	if dryRun {
		verb = "θα προστεθεί σε %d, θα αφαιρεθεί από %d (δοκιμαστικά, δεν άλλαξε κανένας ρόλος)"
	}
	msg := fmt.Sprintf("✅ Ο έλεγχος δραστηριότητας τελείωσε: διαβάστηκαν %d κανάλια και νήματα, καταγράφηκαν %d μέλη. "+
		"Ανενεργά (Ghost): %d· "+verb+".", channels, rep.members, rep.ghosts, rep.added, rep.removed)
	if rep.failed > 0 {
		msg += fmt.Sprintf(" Αποτυχίες: %d.", rep.failed)
	}
	if err != nil {
		msg += fmt.Sprintf("\n⚠️ Διακόπηκε: ```%v```", err)
	}
	h.alertMods(s, "%s", msg)
}

// cmdGhostsScan starts the scan. It is how the index is first filled.
func (h *Handler) cmdGhostsScan(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	dryRun := false
	if o := findOpt(sub.Options, "dry_run"); o != nil {
		dryRun = o.BoolValue()
	}
	if !h.ghostScanning.CompareAndSwap(false, true) {
		h.reply(s, i, "Ο έλεγχος τρέχει ήδη. Θα γράψω το αποτέλεσμα στο κανάλι των logs όταν τελειώσει.")
		return
	}
	h.log.Info("ghosts: scan requested", "user_id", actorID(i), "dry_run", dryRun)
	note := ""
	if dryRun {
		note = " Δοκιμαστικά: θα ενημερώσω τη λίστα αλλά δεν θα αλλάξω κανέναν ρόλο."
	}
	h.reply(s, i, fmt.Sprintf("🔎 Ξεκίνησε ο έλεγχος των μηνυμάτων των τελευταίων %d μηνών. "+
		"Μπορεί να κρατήσει πολλή ώρα· θα γράψω το αποτέλεσμα στο κανάλι των logs.%s", ghostAfterMonths, note))
	go h.safely("ghosts_scan", func() {
		defer h.ghostScanning.Store(false)
		h.runGhostScan(s, dryRun)
	})
}
