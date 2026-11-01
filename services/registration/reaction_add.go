package registration

import (
	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
)

// onReactionAdd implements the legacy restart reaction.
// Both only apply inside the reacting member's own registration channel.
func (h *Handler) onReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if !h.InHomeGuild(r.GuildID) || (s.State.User != nil && r.UserID == s.State.User.ID) {
		return
	}
	ctx, cancel := h.Ctx()
	defer cancel()

	ch, err := h.Channel(ctx, s, r.ChannelID)
	if err != nil {
		return
	}
	// A thread or forum post can carry any name a member likes, so only a
	// plain text channel counts as a register channel.
	owner, ok := bot.UserIDFromChannelName(ch.Name)
	if !ok || ch.Type != discordgo.ChannelTypeGuildText || owner != r.UserID {
		return
	}

	if r.Emoji.Name == restartEmoji {
		if err := h.restartRegistration(s, r.ChannelID, r.UserID, r.GuildID); err != nil {
			h.Log.Error("restart registration failed", "user_id", r.UserID, "error", err)
		}
		return
	}

	// Nothing else is a control here. Reactions never grant roles: the
	// verified role is only ever given by the email link.
	if err := s.MessageReactionRemove(r.ChannelID, r.MessageID, r.Emoji.APIName(), r.UserID); err != nil {
		h.Log.Debug("remove unknown reaction failed", "emoji", r.Emoji.Name, "error", err)
	}
}
