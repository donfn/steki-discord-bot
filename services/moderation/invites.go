package moderation

import (
	"strings"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
	"steki-bot/dc"
	"steki-bot/moderation"
)

var inviteRegex = moderation.InviteRegex

// screenInvites deletes invites to other servers, a common lure from hijacked
// accounts. Moderators may post any invite; anyone may post one to Steki.
// It reports whether the message was removed.
func (h *Handler) screenInvites(s *discordgo.Session, m *discordgo.Message) bool {
	text := moderation.Text(m)
	matches := inviteRegex.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return false
	}
	foreign := false
	for _, match := range matches {
		if !h.isHomeInvite(s, strings.ToLower(match[1]), match[2]) {
			foreign = true
			break
		}
	}
	if !foreign || h.isModerator(s, m.Author.ID, m.ChannelID) {
		return false
	}

	ctx, cancel := h.Ctx()
	defer cancel()
	err := h.Retry(ctx, "message.delete", func() error {
		return s.ChannelMessageDelete(m.ChannelID, m.ID, discordgo.WithContext(ctx))
	})
	if err != nil && !dc.IsMissingResource(err) {
		h.Log.Warn("invite: delete failed", "message_id", m.ID, "error", err)
		return false
	}

	if dm, err := s.UserChannelCreate(m.Author.ID); err == nil {
		h.Send(s, dm.ID, "Το μήνυμά σου στο Steki διαγράφηκε γιατί περιείχε **πρόσκληση σε άλλον Discord server**. "+
			"Μόνο η Ομάδα Διαχείρισης μπορεί να στέλνει προσκλήσεις σε άλλους servers.\n"+
			"**Αν δεν το έστειλες εσύ, άλλαξε άμεσα τον κωδικό σου και ενεργοποίησε το 2FA.**")
	}
	h.AlertMods(s, "🔗 Διαγράφηκε πρόσκληση σε άλλον server από <@%s> στο <#%s>:```%s```",
		m.Author.ID, m.ChannelID, bot.Truncate(bot.Quote(text), 1500))
	h.Log.Info("invite: removed foreign invite", "user_id", m.Author.ID, "channel_id", m.ChannelID)
	return true
}

// isHomeInvite reports whether an invite opens the Steki guild. Results are
// cached per code. A lookup that fails for a reason other than an unknown
// invite is treated as Steki's, so a Discord outage never deletes messages.
func (h *Handler) isHomeInvite(s *discordgo.Session, host, code string) bool {
	if host == "discord.me" || host == "dsc.gg" {
		return false
	}
	h.invitesMu.Lock()
	home, cached := h.invites[code]
	h.invitesMu.Unlock()
	if cached {
		return home
	}

	inv, err := s.Invite(code)
	switch {
	case err == nil:
		home = inv.Guild != nil && h.InHomeGuild(inv.Guild.ID)
	case dc.IsMissingResource(err):
		home = false
	default:
		h.Log.Warn("invite: lookup failed", "code", code, "error", err)
		return true
	}

	h.invitesMu.Lock()
	if len(h.invites) > 5000 {
		clear(h.invites)
	}
	h.invites[code] = home
	h.invitesMu.Unlock()
	return home
}
