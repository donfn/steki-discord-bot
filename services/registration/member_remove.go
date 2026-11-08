package registration

import (
	"errors"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/bot"
	"steki-bot/internal/store"
)

// onMemberRemove cleans up after a member who left or was removed.
//
// The channel is found through the registration row rather than by scanning
// every channel in the guild, which is both cheaper and correct when the
// channel sits in an overflow category.
func (h *Handler) onMemberRemove(s *discordgo.Session, m *discordgo.GuildMemberRemove) {
	if m.User == nil || !h.InHomeGuild(m.GuildID) {
		return
	}

	defer h.LockUser(m.User.ID)()

	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	// discordgo evicts the member from its cache before this handler runs,
	// so the join time comes from the database instead.
	stay := "άγνωστο"
	if member, err := h.st.GetMember(ctx, m.User.ID); err == nil {
		stay = fmt.Sprintf("%.2f ημέρες", time.Since(member.LastJoinAt).Hours()/24)
	}
	h.AlertMods(s, "Ο χρήστης *%s* (<@%s>) έφυγε από τον server μετά από %s.",
		m.User.Username, m.User.ID, stay)

	if err := h.st.MarkMemberLeft(ctx, m.User.ID); err != nil {
		h.Log.Warn("mark member left failed", "user_id", m.User.ID, "error", err)
	}

	reg, err := h.st.OpenRegistration(ctx, m.User.ID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		h.Log.Error("member_remove: lookup registration failed", "user_id", m.User.ID, "error", err)
		return
	}

	h.Audit(m.User.ID, reg.ID, store.EventLeft, map[string]any{"step": reg.Step}, "")
	h.closeAndCleanup(ctx, s, reg, store.OutcomeLeft)
}
