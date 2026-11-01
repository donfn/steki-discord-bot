package moderation

import (
	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
	"steki-bot/rpc"
)

// onMessage screens guild messages. Direct messages are ignored.
//
// Screening runs strongest signal first: blacklisted and phishing links
// (every guild), then in Steki the same message across channels, invites to
// other servers, and the media guard. A removed invite still counts towards
// the media guard, since it is a link.
func (h *Handler) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.GuildID == "" {
		return
	}
	if h.InHomeGuild(m.GuildID) {
		h.Track("discord_message_sent", bot.MessageProps(m.Message))
		go h.Safely("ghost_activity", func() { h.noteActivity(s, m.Message) })
	}
	if m.Author.Bot {
		return
	}
	h.processed.Add(1)
	h.msgs.put(m.Message)

	if h.linkChk.Check(s, m.Message) {
		h.blocked.Add(1)
		return
	}
	if !h.InHomeGuild(m.GuildID) {
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
	if h.screenMedia(s, m) {
		// The web service sees the message too. It must not list what a member
		// who just tripped the guard posted.
		ctx, cancel := h.Ctx()
		defer cancel()
		err := h.web.Call(ctx, rpc.PathDropMessage, rpc.DropMessageRequest{
			MessageID: m.ID, GuildID: m.GuildID, ChannelID: m.ChannelID,
		}, nil)
		if err != nil {
			h.Log.Warn("media guard: tell web to drop the message", "message_id", m.ID, "error", err)
		}
	}
}
