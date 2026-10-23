package events

import (
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
)

// onReady runs on every fresh gateway session (not on resumes).
//
// Background loops are started exactly once, but the reconciler is invoked on
// every Ready: a new session is precisely when GUILD_MEMBER_ADD events may
// have been missed, and the sweep is what recovers them.
func (h *Handler) onReady(s *discordgo.Session, r *discordgo.Ready) {
	for _, g := range r.Guilds {
		h.setGuildID(g.ID)
	}

	h.log.Info("gateway ready",
		"user", r.User.Username, "guilds", len(r.Guilds), "session", r.SessionID)

	h.startOnce.Do(func() {
		h.goLoop("status", 30*time.Second, func() { h.updateStatus(s) })
		h.startMaintenance(s)
	})

	go h.safely("reconcile_on_ready", func() {
		if err := h.registerCommands(s); err != nil {
			h.log.Error("register slash commands failed", "error", err)
		}
		h.reconcile(s)
	})
}

// onGuildCreate learns the guild ID, which the bot is otherwise never told.
func (h *Handler) onGuildCreate(_ *discordgo.Session, g *discordgo.GuildCreate) {
	h.setGuildID(g.ID)
}

// onDisconnect and onResumed make gateway health visible in the logs, since
// silent reconnects were masking dropped joins.
func (h *Handler) onDisconnect(_ *discordgo.Session, _ *discordgo.Disconnect) {
	h.log.Warn("gateway disconnected")
}

func (h *Handler) onResumed(s *discordgo.Session, _ *discordgo.Resumed) {
	h.log.Info("gateway resumed")
	go h.safely("reconcile_on_resume", func() { h.reconcile(s) })
}

// updateStatus rotates the presence text.
//
// The previous implementation ticked every ten seconds regardless of
// connection state, producing a steady stream of
// "rotateStatus: no websocket connection exists" errors while reconnecting.
func (h *Handler) updateStatus(s *discordgo.Session) {
	s.RLock()
	ready := s.DataReady
	s.RUnlock()
	if !ready {
		return
	}

	var status string
	switch h.statusIndex.Add(1) % 3 {
	case 0:
		status = "Uptime: " + formatUptime(time.Since(h.bootTime))
	case 1:
		status = fmt.Sprintf("Κακόβουλα μηνύματα: %d", h.blocked.Load())
	case 2:
		status = fmt.Sprintf("Επεξεργασμένα μηνύματα: %d", h.processed.Load())
	}

	if err := s.UpdateGameStatus(0, status); err != nil {
		h.log.Debug("update status failed", "error", err)
	}
}

// formatUptime renders a duration as "Nd Nh Nm".
func formatUptime(d time.Duration) string {
	return fmt.Sprintf("%dd %dh %dm", int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60)
}
