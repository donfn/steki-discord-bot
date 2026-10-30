package events

import (
	"github.com/bwmarrin/discordgo"
)

// onReactionRemove reports a reaction a member took back.
func (h *Handler) onReactionRemove(s *discordgo.Session, r *discordgo.MessageReactionRemove) {
	if !h.inHomeGuild(r.GuildID) || (s.State.User != nil && r.UserID == s.State.User.ID) {
		return
	}
	h.track("discord_reaction_removed", reactionProps(r.MessageReaction))
}

// reactionProps describes a reaction by its emoji and where it was left.
func reactionProps(r *discordgo.MessageReaction) map[string]any {
	return map[string]any{
		"guild_id": r.GuildID, "channel_id": r.ChannelID,
		"emoji": r.Emoji.Name, "emoji_custom": r.Emoji.ID != "",
	}
}

// onReactionAdd implements the legacy restart reaction.
// Both only apply inside the reacting member's own registration channel.
func (h *Handler) onReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if !h.inHomeGuild(r.GuildID) || (s.State.User != nil && r.UserID == s.State.User.ID) {
		return
	}
	h.track("discord_reaction_added", reactionProps(r.MessageReaction))

	ctx, cancel := h.ctx()
	defer cancel()

	ch, err := h.channel(ctx, s, r.ChannelID)
	if err != nil {
		return
	}
	// A thread or forum post can carry any name a member likes, so only a
	// plain text channel counts as a register channel.
	owner, ok := userIDFromChannelName(ch.Name)
	if !ok || ch.Type != discordgo.ChannelTypeGuildText || owner != r.UserID {
		return
	}

	if r.Emoji.Name == restartEmoji {
		if err := h.restartRegistration(s, r.ChannelID, r.UserID, r.GuildID); err != nil {
			h.log.Error("restart registration failed", "user_id", r.UserID, "error", err)
		}
		return
	}

	// Nothing else is a control here. Reactions never grant roles: the
	// verified role is only ever given by the email link.
	if err := s.MessageReactionRemove(r.ChannelID, r.MessageID, r.Emoji.APIName(), r.UserID); err != nil {
		h.log.Debug("remove unknown reaction failed", "emoji", r.Emoji.Name, "error", err)
	}
}
