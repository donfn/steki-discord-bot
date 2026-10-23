package events

import (
	"github.com/bwmarrin/discordgo"
)

// onReactionAdd implements the restart control and the interest-role picker.
// Both only apply inside the reacting member's own registration channel.
func (h *Handler) onReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if !h.inHomeGuild(r.GuildID) || (s.State.User != nil && r.UserID == s.State.User.ID) {
		return
	}

	ctx, cancel := h.ctx()
	defer cancel()

	ch, err := h.channel(ctx, s, r.ChannelID)
	if err != nil {
		return
	}
	owner, ok := userIDFromChannelName(ch.Name)
	if !ok || owner != r.UserID {
		return
	}

	if r.Emoji.Name == restartEmoji {
		if err := h.restartRegistration(s, r.ChannelID, r.UserID, r.GuildID); err != nil {
			h.log.Error("restart registration failed", "user_id", r.UserID, "error", err)
		}
		return
	}

	roleID, known := reactionRoles[r.Emoji.Name]
	if !known {
		// APIName is "name:id" for custom emoji, which the endpoint requires.
		if err := s.MessageReactionRemove(r.ChannelID, r.MessageID, r.Emoji.APIName(), r.UserID); err != nil {
			h.log.Debug("remove unknown reaction failed", "emoji", r.Emoji.Name, "error", err)
		}
		return
	}

	if err := h.grantRole(ctx, s, r.GuildID, r.UserID, roleID); err != nil {
		h.log.Error("add interest role failed", "user_id", r.UserID, "role_id", roleID, "error", err)
	}
}
