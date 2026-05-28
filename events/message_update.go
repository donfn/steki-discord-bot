package events

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnMessageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	if m.BeforeUpdate == nil || m.BeforeUpdate.Content == "" || m.Content == "" {
		return
	}
	if m.BeforeUpdate.Content == m.Content {
		return
	}

	before := strings.ReplaceAll(m.BeforeUpdate.Content, "`", "\"")
	after := strings.ReplaceAll(m.Content, "`", "\"")

	h.logger.Log(s, fmt.Sprintf(
		"Ο χρήστης <@%s> επεξεργάστηκε στο κανάλι <#%s> αυτό το μήνυμα:\n```%s```*και η νέα έκδοσή του είναι:*```%s```",
		m.Author.ID, m.ChannelID, before, after,
	))
}
