package events

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnMemberRemove(s *discordgo.Session, m *discordgo.GuildMemberRemove) {
	user := m.User

	// JoinedAt is populated from gateway cache when GUILD_MEMBERS intent is enabled
	joinedAt := m.JoinedAt
	if joinedAt.IsZero() {
		if cached, err := s.State.Member(m.GuildID, user.ID); err == nil {
			joinedAt = cached.JoinedAt
		}
	}

	var daysSince string
	if !joinedAt.IsZero() {
		days := time.Since(joinedAt).Hours() / 24
		daysSince = fmt.Sprintf("%.2f ημέρες", days)
	} else {
		daysSince = "άγνωστο"
	}

	h.logger.Log(s, fmt.Sprintf(
		"Ο χρήστης *%s* (<@%s>) αποχώρησε από τον σέρβερ. Ήταν στον σέρβερ για %s.",
		user.Username, user.ID, daysSince,
	))

	channels, err := s.GuildChannels(m.GuildID)
	if err != nil {
		log.Println("OnMemberRemove: get channels:", err)
		return
	}
	for _, ch := range channels {
		if strings.Contains(ch.Name, fmt.Sprintf("register-%s", user.ID)) {
			if _, err := s.ChannelDelete(ch.ID); err != nil {
				log.Println("OnMemberRemove: delete channel:", err)
			}
		}
	}
}
