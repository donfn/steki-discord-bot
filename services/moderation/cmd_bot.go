package moderation

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
)

// cmdBotStats shows this service's health and each of the others', so a service
// that is down shows up here even though its own commands would not answer.
func (h *Handler) cmdBotStats(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "**Κατάσταση bot**\n• Uptime: %s\n", formatUptime(time.Since(h.BootTime)))
	fmt.Fprintf(&b, "• Μηνύματα: %d · Μπλοκαρισμένα: %d\n", h.processed.Load(), h.blocked.Load())
	if err := h.st.Ping(ctx); err != nil {
		fmt.Fprintf(&b, "• 🔴 Βάση προστασίας: %v\n", err)
	} else {
		fmt.Fprintf(&b, "• 🟢 Βάσεις προστασίας: %.1f MB\n", float64(h.st.FileSize()+h.activity.FileSize())/(1<<20))
	}
	if counts, err := h.st.CountEventsByKind(ctx, time.Now().Add(-24*time.Hour)); err == nil {
		b.WriteString(bot.FormatEventCounts(counts))
	}
	b.WriteString(h.statsOf(ctx, "registration", h.registration))
	b.WriteString(h.statsOf(ctx, "web", h.web))
	h.Reply(s, i, b.String())
}
