package events

import (
	"context"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/store"
)

// The ghost role marks members who have not sent a message for three months.
// The member_activity table indexes every member of the guild with the time of
// their last message and whether they hold the role. Nothing reads channel
// history: a message arriving on the gateway stamps its author and takes the
// role away, joins and leaves add and remove rows, the member list
// /reg reconcile fetches keeps the roster whole, and a sweep over the
// table gives the role to whoever has passed three months of silence. The
// clock of a member who has not posted yet starts at their join time, so a
// newcomer is not a ghost.

const (
	// ghostAfterMonths is how long a member may stay silent before they are a
	// ghost.
	ghostAfterMonths = 3

	// ghostSweepEvery is how often the index is checked for members who have
	// gone quiet. Silence is measured in months, so hourly is generous.
	ghostSweepEvery = time.Hour

	// maxGhostsPerSweep bounds how many roles one sweep hands out, so a backlog
	// drains over a few ticks instead of in one burst.
	maxGhostsPerSweep = 50

	// ghostMaxConsecutiveFailures stops a sweep that is failing every call, as
	// it does when the role sits above the bot's own.
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

// noteActivity stamps the author of a gateway message in the index and, if
// they hold the ghost role, takes it away. The Member that comes with a guild
// message carries the author's roles and join time.
func (h *Handler) noteActivity(s *discordgo.Session, m *discordgo.Message) {
	if !countsAsActivity(m) {
		return
	}
	now := time.Now()
	joined := now
	hasRole := false
	if m.Member != nil {
		joined = joinedAt(m.Member, now)
		hasRole = slices.Contains(m.Member.Roles, ghostRoleID)
	}

	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()
	if err := h.st.RecordMessage(ctx, m.Author.ID, now, joined, hasRole); err != nil {
		h.log.Error("ghosts: record message", "user_id", m.Author.ID, "error", err)
		return
	}
	if !hasRole {
		return
	}
	if err := h.setGhostRole(ctx, s, m.GuildID, m.Author.ID, false); err != nil {
		h.log.Warn("ghosts: remove role", "user_id", m.Author.ID, "error", err)
		return
	}
	if err := h.st.SetGhost(ctx, m.Author.ID, false); err != nil {
		h.log.Error("ghosts: record role", "user_id", m.Author.ID, "error", err)
	}
}

// noteJoin adds a member to the index when they join.
func (h *Handler) noteJoin(m *discordgo.Member) {
	ctx, cancel := h.ctx()
	defer cancel()
	if err := h.st.RecordJoin(ctx, m.User.ID, joinedAt(m, time.Now())); err != nil {
		h.log.Error("ghosts: record join", "user_id", m.User.ID, "error", err)
	}
}

// noteLeave drops a member who left from the index.
func (h *Handler) noteLeave(userID string) {
	ctx, cancel := h.ctx()
	defer cancel()
	if err := h.st.DeleteMemberActivity(ctx, userID); err != nil {
		h.log.Error("ghosts: forget member", "user_id", userID, "error", err)
	}
}

// syncActivityRoster makes the index list exactly the members of a full member
// list, adding any join the gateway missed and dropping any leave it missed.
// It leaves every last message as it is.
func (h *Handler) syncActivityRoster(ctx context.Context, members []*discordgo.Member, listedAt time.Time) {
	now := time.Now()
	entries := make([]store.ActivityEntry, 0, len(members))
	for _, m := range members {
		if m.User == nil || m.User.Bot {
			continue
		}
		entries = append(entries, store.ActivityEntry{UserID: m.User.ID, JoinedAt: joinedAt(m, now)})
	}
	if err := h.st.SyncMemberActivity(ctx, listedAt, entries); err != nil {
		h.log.Error("ghosts: sync roster", "error", err)
	}
}

// sweepGhosts gives the role to members who have been silent for three months.
func (h *Handler) sweepGhosts(s *discordgo.Session) {
	guildID := h.guildID()
	if guildID == "" {
		return
	}
	ctx, cancel := h.ctxTimeout(reapTimeout)
	defer cancel()

	ids, err := h.st.GhostCandidates(ctx, ghostCutoff(time.Now()), maxGhostsPerSweep)
	if err != nil {
		h.log.Error("ghosts: list candidates", "error", err)
		return
	}
	streak := 0
	for _, id := range ids {
		err := h.setGhostRole(ctx, s, guildID, id, true)
		switch {
		case dc.IsMissingResource(err):
			// Gone from the guild without the gateway telling us.
			h.noteLeave(id)
			continue
		case err != nil:
			h.log.Warn("ghosts: add role", "user_id", id, "error", err)
			if streak++; streak >= ghostMaxConsecutiveFailures {
				h.urgentModsOnce(s, "ghost-role", "⚠️ Δεν μπορώ να δώσω τον ρόλο Ghost (%d αποτυχίες στη σειρά): ```%v```", streak, err)
				return
			}
			continue
		}
		streak = 0
		if err := h.st.SetGhost(ctx, id, true); err != nil {
			h.log.Error("ghosts: record role", "user_id", id, "error", err)
		}
		h.log.Info("ghosts: role added", "user_id", id)
	}
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
