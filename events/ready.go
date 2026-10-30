package events

import (
	"fmt"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
)

// onReady runs on every fresh gateway session (not on resumes) and starts the
// background loops exactly once. The loops idle until the Steki guild is known.
func (h *Handler) onReady(s *discordgo.Session, r *discordgo.Ready) {
	h.log.Info("gateway ready",
		"user", r.User.Username, "guilds", len(r.Guilds), "session", r.SessionID)

	h.startOnce.Do(func() {
		h.goLoop("status", 30*time.Second, func() { h.updateStatus(s) })
		h.goLoop("phishing_feed", phishRefreshEvery, h.refreshPhishFeed)
		h.goLoop("duplicate_sweep", dupWindow, func() { h.dups.sweep(time.Now()) })
		h.startMaintenance(s)
	})
}

// onGuildCreate fires for every guild after each new gateway session. The
// Steki guild is recognised by owning the register category: the bot is in
// more than one server, so the ID cannot simply be taken from the first event.
//
// A new session is exactly when joins may have been missed, but nothing is
// repaired here: that is /reg reconcile, which a moderator runs when they know
// the bot was away. All that happens on connecting is what the bot cannot work
// without: the commands are installed, once per process, and the material
// worker starts.
func (h *Handler) onGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	if !isHomeGuild(g.Guild) {
		return
	}
	h.guild.Store(g.ID)
	h.log.Info("home guild available", "guild_id", g.ID, "name", g.Name)

	if !h.commandsSynced.Load() {
		go h.safely("register_commands", func() {
			if err := h.registerCommands(s); err != nil {
				h.log.Error("register slash commands failed", "error", err)
				return
			}
			h.commandsSynced.Store(true)
		})
	}
	h.materialsOnce.Do(func() { h.startMaterials(s) })
}

// startMaterials starts the material worker once the bucket has been
// confirmed reachable. Nothing here reads channel history or touches a message
// already in Discord: the stickies, the cards for held files and reports, and
// old history are brought up to date by /materials stickies, /materials cards
// and /materials scan. The worker only picks up the work the database holds.
func (h *Handler) startMaterials(s *discordgo.Session) {
	if h.mat == nil {
		return
	}
	ctx, cancel := h.ctx()
	err := h.bucket.Check(ctx)
	cancel()
	if err != nil {
		h.log.Error("material backup off: bucket unreachable", "error", err)
		h.urgentMods(s, "⚠️ Δεν έχω πρόσβαση στο bucket των αντιγράφων (%v). Η αποθήκευση υλικού είναι απενεργοποιημένη μέχρι την επόμενη επανεκκίνηση.", err)
		return
	}
	h.log.Info("material backup enabled")
	h.matLive.Store(true)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.safely("materials_worker", func() { h.mat.Run(h.base) })
	}()
}

// isHomeGuild reports whether g is the Steki guild.
func isHomeGuild(g *discordgo.Guild) bool {
	return g != nil && slices.ContainsFunc(g.Channels, func(c *discordgo.Channel) bool {
		return c.ID == config.RegisterCategoryID
	})
}

// onDisconnect and onResumed make gateway health visible in the logs, since
// silent reconnects were masking dropped joins.
func (h *Handler) onDisconnect(_ *discordgo.Session, _ *discordgo.Disconnect) {
	h.log.Warn("gateway disconnected")
}

// A resume replays the events missed while the connection was down, so there
// is nothing to catch up on.
func (h *Handler) onResumed(_ *discordgo.Session, _ *discordgo.Resumed) {
	h.log.Info("gateway resumed")
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
