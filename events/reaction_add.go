package events

import (
	"fmt"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if r.UserID == s.State.User.ID {
		return
	}

	ch, err := s.State.Channel(r.ChannelID)
	if err != nil {
		ch, err = s.Channel(r.ChannelID)
		if err != nil {
			return
		}
	}

	if ch.Name != fmt.Sprintf("register-%s", r.UserID) {
		return
	}

	emoji := r.Emoji.Name

	if emoji == "🔄" {
		if err := h.restartRegistration(s, r.ChannelID, r.UserID); err != nil {
			log.Println("OnReactionAdd: restartRegistration:", err)
		}
		return
	}

	roleID, ok := reactionRoles[emoji]
	if !ok {
		s.MessageReactionRemove(r.ChannelID, r.MessageID, emoji, r.UserID)
		return
	}

	if err := s.GuildMemberRoleAdd(r.GuildID, r.UserID, roleID); err != nil {
		log.Println("OnReactionAdd: add role:", err)
		return
	}

	reg, _ := h.regRepo.FindByUser(r.UserID)
	h.completeRegistration(s, r.ChannelID, reg)
}

func (h *Handler) restartRegistration(s *discordgo.Session, channelID, userID string) error {
	if err := h.regRepo.DeleteByUser(userID); err != nil {
		return err
	}

	msgs, err := s.ChannelMessages(channelID, 100, "", "", "")
	if err == nil && len(msgs) > 0 {
		ids := make([]string, len(msgs))
		for i, msg := range msgs {
			ids[i] = msg.ID
		}
		s.ChannelMessagesBulkDelete(channelID, ids)
	}

	s.ChannelMessageSend(channelID, fmt.Sprintf(
		"**Γειά σου <@%s>, καλωσόρισες στο Steki!** \nΘα χρειαστεί να επιβεβαιώσουμε την φοιτητική σου ιδιότητα.\n\n"+
			"Είναι πολύ απλό: \n**Θα σου στείλω έναν κωδικό στο ακαδημαϊκό σου email και εσύ θα πρέπει να μου τον στείλεις πίσω.**",
		userID,
	))

	time.AfterFunc(1*time.Second, func() {
		msg, err := s.ChannelMessageSend(channelID,
			"Αν κολλήσεις σε κάποιο από τα βήματα μπορείς να πατήσεις το :arrows_counterclockwise: κάτω από αυτό το μήνυμα "+
				"και θα ξανα-αρχίσουμε τη διαδικασία από την αρχή. Εναλλακτικά, στείλε στο <#939177847933780018>.",
		)
		if err == nil {
			s.MessageReactionAdd(channelID, msg.ID, "🔄")
		}
	})

	time.AfterFunc(2*time.Second, func() {
		s.ChannelMessageSend(channelID,
			"**Για να συνεχίσουμε, στείλε μου εδώ το ακαδημαϊκό σου email. "+
				"Θα πρέπει να είναι της μορφής *thXXXXXX@edu.hmu.gr* (αυτό που σου έδωσε η γραμματεία κατά την εγγραφή)**.",
		)
	})

	return nil
}
