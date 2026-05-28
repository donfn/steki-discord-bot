package events

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("Logged in as %s", r.User.Username)
	h.startOnce.Do(func() {
		bootTime := time.Now()
		go h.rotateStatus(s, bootTime)
		go h.spamUnregistered(s)
	})
}

func (h *Handler) rotateStatus(s *discordgo.Session, bootTime time.Time) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	i := 0
	for range ticker.C {
		var status string
		switch i {
		case 0:
			d := time.Since(bootTime)
			days := int(d.Hours()) / 24
			hours := int(d.Hours()) % 24
			mins := int(d.Minutes()) % 60
			status = fmt.Sprintf("Bot Uptime: %dd %dh %dm", days, hours, mins)
		case 2:
			status = fmt.Sprintf("Κακόβουλα μηνύματα που έχουν μπλοκαριστεί: %d", h.getBlocked())
		case 3:
			status = fmt.Sprintf("Επεξεργασμένα μηνύματα: %d", h.getProcessed())
		}
		if status != "" {
			if err := s.UpdateGameStatus(0, status); err != nil {
				log.Println("rotateStatus:", err)
			}
		}
		i = (i + 1) % 4
	}
}

func (h *Handler) spamUnregistered(s *discordgo.Session) {
	ticker := time.NewTicker(20 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		for _, guild := range s.State.Guilds {
			channels, err := s.GuildChannels(guild.ID)
			if err != nil {
				log.Println("spamUnregistered: get channels:", err)
				continue
			}
			for _, ch := range channels {
				if !strings.HasPrefix(ch.Name, "register-") {
					continue
				}
				parts := strings.SplitN(ch.Name, "-", 2)
				if len(parts) < 2 {
					continue
				}
				userID := parts[1]
				dm, err := s.UserChannelCreate(userID)
				if err != nil {
					continue
				}
				msg := fmt.Sprintf(
					"🆘 Το Steki για εσένα τώρα φαίνεται κενό και αυτό είναι γιατί δεν έχεις γραφτεί.\n\n"+
						"ℹ️ Για να ολοκληρώσεις την εγγραφή σου στο Steki, μπές εδώ → <#%s>!\n\n"+
						"💁🏻 Αν αντιμετωπίζεις κάποιο πρόβλημα ή δεν ξέρεις τι να κάνεις, στείλε στο <#939177847933780018>.\n"+
						"Έχε υπόψη ότι είμαι απλά ένα πρόγραμμα.",
					ch.ID,
				)
				s.ChannelMessageSend(dm.ID, msg)
			}
		}
	}
}
