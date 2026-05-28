package events

import (
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}

	h.incrProcessed()

	deleted, err := h.linkChk.Check(s, m.Message)
	if err != nil {
		log.Println("linkcheck:", err)
	}
	if deleted {
		h.incrBlocked()
		return
	}

	if m.ChannelID == h.cfg.LogChannelID {
		h.handleAdminCommand(s, m)
		return
	}

	ch, err := s.State.Channel(m.ChannelID)
	if err != nil {
		ch, err = s.Channel(m.ChannelID)
		if err != nil {
			return
		}
	}

	if strings.HasPrefix(ch.Name, "register-") {
		h.handleRegistration(s, m, ch)
	}
}
