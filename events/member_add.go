package events

import (
	"fmt"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (h *Handler) OnMemberAdd(s *discordgo.Session, m *discordgo.GuildMemberAdd) {
	user := m.User

	ch, err := s.GuildChannelCreateComplex(m.GuildID, discordgo.GuildChannelCreateData{
		Name:     fmt.Sprintf("register-%s", user.ID),
		Type:     discordgo.ChannelTypeGuildText,
		Topic:    "Ολοκλήρωσε την εγγραφή σου για να μπορέσεις να δείς όλα τα κανάλια στο Steki!",
		ParentID: h.cfg.RegisterCategoryID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:   m.GuildID,
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionViewChannel,
			},
			{
				ID:    user.ID,
				Type:  discordgo.PermissionOverwriteTypeMember,
				Allow: discordgo.PermissionViewChannel,
			},
			{
				ID:    modViewRoleID,
				Type:  discordgo.PermissionOverwriteTypeRole,
				Allow: discordgo.PermissionViewChannel,
			},
		},
	})
	if err != nil {
		log.Println("OnMemberAdd: create channel:", err)
		return
	}

	welcome := fmt.Sprintf(":wave: Γειά σου, <@%s>! Πάτα εδώ για να γραφτείς στο Steki → <#%s>.", user.ID, ch.ID)
	s.ChannelMessageSend(h.cfg.WelcomeChannelID, welcome)

	if dm, err := s.UserChannelCreate(user.ID); err == nil {
		s.ChannelMessageSend(dm.ID, welcome)
	}

	s.ChannelMessageSend(ch.ID, fmt.Sprintf(
		"**Γειά σου <@%s>, καλωσόρισες στο Steki!** \nΘα χρειαστεί να επιβεβαιώσουμε την φοιτητική σου ιδιότητα.\n\n"+
			"Είναι πολύ απλό: \n**Θα σου στείλω έναν κωδικό στο ακαδημαϊκό σου email και εσύ θα πρέπει να μου τον γράψεις εδώ.**",
		user.ID,
	))

	time.AfterFunc(1*time.Second, func() {
		msg, err := s.ChannelMessageSend(ch.ID,
			"Αν κολλήσεις σε κάποιο από τα βήματα, μπορείς να κάνεις react με :arrows_counterclockwise: στο μήνυμα αυτό "+
				"και θα αρχίσει η διαδικασία από την αρχή. Εναλλακτικά, στείλε στο <#939177847933780018>.\n"+
				"Το email σου θα ανωνυμοποιηθεί (sha256), για αποκλειστική χρήση στην παρούσα διαδικασία.",
		)
		if err == nil {
			s.MessageReactionAdd(ch.ID, msg.ID, "🔄")
		}
	})

	time.AfterFunc(2*time.Second, func() {
		s.ChannelMessageSend(ch.ID,
			"**Για να συνεχίσουμε, στείλε μου εδώ το ακαδημαϊκό σου email. "+
				"Θα πρέπει να είναι της μορφής *thXXXXXX@edu.hmu.gr* (αυτό που σου έδωσε η γραμματεία κατά την εγγραφή)**.\n\n"+
				":new: Τώρα φοιτητές από εξωτερικά τμήματα (csd.uoc.gr) μπορούν να γραφτούν ως Verified Outsiders.",
		)
	})
}
