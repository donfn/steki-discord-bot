package events

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnMessageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	// BeforeDelete is only set if the message was cached before deletion
	if m.BeforeDelete == nil || m.BeforeDelete.Author == nil {
		return
	}
	author := m.BeforeDelete.Author
	if author.Bot {
		return
	}

	member, err := s.GuildMember(m.GuildID, author.ID)
	if err != nil {
		log.Println("OnMessageDelete: get member:", err)
		return
	}
	if hasRole(member, staffRoleID) || hasRole(member, modRoleID) {
		return
	}

	// Attempt to find the executor via audit log (matching channel and recency, like the JS version)
	executorID := author.ID
	logs, err := s.GuildAuditLog(m.GuildID, "", "", int(discordgo.AuditLogActionMessageDelete), 10)
	if err == nil {
		const discordEpoch = int64(1420070400000)
		for _, entry := range logs.AuditLogEntries {
			if entry.TargetID != author.ID {
				continue
			}
			if entry.Options != nil && entry.Options.ChannelID != m.ChannelID {
				continue
			}
			id, _ := strconv.ParseInt(entry.ID, 10, 64)
			ms := (id>>22) + discordEpoch
			entryTime := time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
			if time.Since(entryTime) > 20*time.Second {
				continue
			}
			executorID = entry.UserID
			break
		}
	}

	h.logger.Log(s, fmt.Sprintf(
		"Ο χρήστης <@%s> διέγραψε από το κανάλι <#%s> αυτό το μήνυμα:\n```%s```",
		executorID, m.ChannelID, m.BeforeDelete.Content,
	))
}
