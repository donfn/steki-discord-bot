package events

import (
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) handleAdminCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	member, err := s.GuildMember(m.GuildID, m.Author.ID)
	if err != nil || !hasRole(member, staffRoleID) {
		return
	}

	parts := strings.Fields(m.Content)
	if len(parts) == 0 {
		return
	}

	_ = s.ChannelMessageDelete(m.ChannelID, m.ID)

	switch parts[0] {
	case "addEmail":
		if len(parts) != 2 {
			h.logger.Log(s, "Εντολή addEmail: προσθέτει ένα email στην db. Χρήση: ```addEmail th00000@edu.hmu.gr```")
			return
		}
		hash := hashSHA256(parts[1])
		exists, _ := h.regRepo.EmailExists(hash)
		if exists {
			h.logger.Log(s, "Αυτό το email υπάρχει ήδη στην db.")
			return
		}
		if err := h.regRepo.AddEmail(hash); err != nil {
			log.Println("addEmail:", err)
			return
		}
		h.logger.Log(s, "Το email `Hash: "+hash+"` προστέθηκε στην db.")

	case "removeEmail":
		if len(parts) != 2 {
			h.logger.Log(s, "Εντολή removeEmail: αφαιρεί ένα email από την db. Χρήση: ```removeEmail th00000@edu.hmu.gr```")
			return
		}
		hash := hashSHA256(parts[1])
		exists, _ := h.regRepo.EmailExists(hash)
		if !exists {
			h.logger.Log(s, "Αυτό το email δεν υπάρχει στην db.")
			return
		}
		if err := h.regRepo.RemoveEmail(hash); err != nil {
			log.Println("removeEmail:", err)
			return
		}
		h.logger.Log(s, "Το email `Hash: "+hash+"` αφαιρέθηκε από την db.")

	case "removeHash":
		if len(parts) != 2 {
			h.logger.Log(s, "Εντολή removeHash: αφαιρεί ένα hash από την db. Χρήση: ```removeHash <hash>```")
			return
		}
		exists, _ := h.regRepo.EmailExists(parts[1])
		if !exists {
			h.logger.Log(s, "Αυτό το hash δεν υπάρχει στην db.")
			return
		}
		if err := h.regRepo.RemoveEmail(parts[1]); err != nil {
			log.Println("removeHash:", err)
			return
		}
		h.logger.Log(s, "Το hash `"+parts[1]+"` αφαιρέθηκε από την db.")

	}
}
