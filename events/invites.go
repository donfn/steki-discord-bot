package events

import (
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/moderation"
)

// inviteRegex matches Discord invite links. Group 2 is the code when the host
// is Discord's own, which lets the bot ask Discord which server it opens;
// vanity redirectors (discord.me, dsc.gg) cannot be resolved and always count
// as foreign.
var inviteRegex = regexp.MustCompile(
	`(?i)\b(discord(?:app)?\.com/invite|discord\.gg|discord\.me|dsc\.gg)/([a-z0-9-]+)`)

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

	ctx, cancel := h.ctx()
	defer cancel()
	err := h.retry(ctx, "message.delete", func() error {
		return s.ChannelMessageDelete(m.ChannelID, m.ID, discordgo.WithContext(ctx))
	})
	if err != nil && !dc.IsMissingResource(err) {
		h.log.Warn("invite: delete failed", "message_id", m.ID, "error", err)
		return false
	}

	if dm, err := s.UserChannelCreate(m.Author.ID); err == nil {
		h.send(s, dm.ID, "Το μήνυμά σου στο Steki διαγράφηκε γιατί περιείχε **πρόσκληση σε άλλον Discord server**. "+
			"Μόνο η Ομάδα Διαχείρισης μπορεί να στέλνει προσκλήσεις σε άλλους servers.\n"+
			"**Αν δεν το έστειλες εσύ, άλλαξε άμεσα τον κωδικό σου και ενεργοποίησε το 2FA.**")
	}
	h.alertMods(s, "🔗 Διαγράφηκε πρόσκληση σε άλλον server από <@%s> στο <#%s>:```%s```",
		m.Author.ID, m.ChannelID, truncate(quote(text), 1500))
	h.log.Info("invite: removed foreign invite", "user_id", m.Author.ID, "channel_id", m.ChannelID)
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
		home = inv.Guild != nil && h.inHomeGuild(inv.Guild.ID)
	case dc.IsMissingResource(err):
		home = false
	default:
		h.log.Warn("invite: lookup failed", "code", code, "error", err)
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
