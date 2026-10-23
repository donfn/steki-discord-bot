package events

import (
	"github.com/bwmarrin/discordgo"
)

// onMessage screens guild messages for blacklisted links and drives the
// registration flow inside the Steki guild's register channels. Direct
// messages are ignored.
func (h *Handler) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || m.GuildID == "" {
		return
	}
	h.processed.Add(1)

	if h.linkChk.Check(s, m.Message) {
		h.blocked.Add(1)
		return
	}
	if !h.inHomeGuild(m.GuildID) {
		return
	}

	ctx, cancel := h.ctx()
	ch, err := h.channel(ctx, s, m.ChannelID)
	cancel()
	if err != nil || !isRegisterChannel(ch) {
		return
	}
	h.handleRegistration(s, m)
}
