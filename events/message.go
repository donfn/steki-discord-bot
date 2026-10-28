package events

import (
	"github.com/bwmarrin/discordgo"
)

// onMessage screens guild messages, then drives the registration flow inside
// the Steki guild's register channels. Direct messages are ignored.
//
// Screening runs strongest signal first: blacklisted and phishing links
// (every guild), then in Steki the same message across channels, invites to
// other servers, and the media guard. A removed invite still counts towards
// the media guard, since it is a link.
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
	if h.screenDuplicates(s, m) {
		h.blocked.Add(1)
		return
	}
	removed := h.screenInvites(s, m.Message)
	if removed {
		h.blocked.Add(1)
	}
	if h.screenMedia(s, m) || removed {
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
