package events

import (
	"crypto/sha256"
	"fmt"
	"math/rand"

	"github.com/bwmarrin/discordgo"
	"steki-bot/registration"
)

func hashSHA256(input string) string {
	sum := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", sum)
}

func generateCode() string {
	return fmt.Sprintf("%06d", rand.Intn(1000000))
}

func hasRole(member *discordgo.Member, roleID string) bool {
	for _, r := range member.Roles {
		if r == roleID {
			return true
		}
	}
	return false
}

func (h *Handler) completeRegistration(s *discordgo.Session, channelID string, reg *registration.Registration) {
	if _, err := s.ChannelDelete(channelID); err != nil {
		// channel may already be gone
		_ = err
	}
	if reg != nil {
		_ = h.regRepo.Delete(reg.ID)
	}
}
