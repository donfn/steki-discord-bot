package events

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// rulesEmbeds are the server rules, the note about pinned messages and the
// role list, as one embed each so they read as separate messages.
func rulesEmbeds() []*discordgo.MessageEmbed {
	return []*discordgo.MessageEmbed{
		{
			Title:       "Κανόνες του server",
			Description: "Ο server λειτουργεί σαν δημόσιο φόρουμ. Μην υποθέτεις ότι υπάρχει ανωνυμία ή ιδιωτικότητα στις συζητήσεις.",
			Color:       stickyColor,
			Fields: []*discordgo.MessageEmbedField{
				{Name: "Τι ισχύει", Value: "" +
					"• Όχι spam.\n" +
					"• Περιεχόμενο που αγγίζει τα όρια του ακραίου κρίνεται από τους admins. Γενικά, μη γράφετε πράγματα που μπορούν να εκθέσουν εσάς ή τους διαχειριστές.\n" +
					"• Απαγορεύονται υπαινιγμοί, προσβολές και ονοματολογία για καθηγητές, φοιτητές ή άλλα πρόσωπα.\n" +
					"• Δεν είμαστε υποχρεωμένοι να σε κρατήσουμε στον server. Αν κρίνουμε ότι είχες ή θα έχεις παραβατική συμπεριφορά, δεν θα σου επιτραπεί η πρόσβαση."},
				{Name: "Αν παραβιάσεις τους κανόνες", Value: "" +
					"Μπαίνεις σε προσωρινό mute: διαβάζεις, αλλά δεν στέλνεις μηνύματα, και τα μηνύματά σου διαγράφονται. " +
					"Αν συνεχίσεις, αφαιρείσαι από τον server."},
				{Name: "Quiz και εξετάσεις", Value: "" +
					"Όταν ανεβάζεις quiz, φρόντισε να μην περιέχει απαντήσεις. Αν ανεβάζεις π.χ. ένα quiz από το eclass, " +
					"μαύρισε με κάποιο πρόγραμμα τις απαντήσεις σου στο screenshot."},
			},
		},
		{
			Title: "Διαχείριση",
			Description: "Θέλουμε ο server να μείνει ενεργός και μετά την αποχώρησή μας από το ΕΛΜΕΠΑ. " +
				"Αν σε ενδιαφέρει να γίνεις admin ή mod, στείλε μας μήνυμα.",
			Color: stickyColor,
		},
		{
			Title:       "📌 Υλικό στα pinned",
			Description: "Στα pinned μηνύματα κάθε καναλιού υπάρχει χρήσιμο υλικό για το αντίστοιχο μάθημα.",
			Color:       stickyColor,
		},
		{
			Title: "Ρόλοι",
			Color: stickyColor,
			Fields: []*discordgo.MessageEmbedField{
				{Name: "Διαχείριση", Value: fmt.Sprintf(""+
					"<@&%s> Οι διαχειριστές του server. Μπορούν να κάνουν τα πάντα.\n"+
					"<@&%s> Διαγράφουν μηνύματα, δημιουργούν και επεξεργάζονται κανάλια, και κάνουν ban ή time-out σε χρήστες.",
					staffRoleID, modRoleID)},
				{Name: "Μέλη", Value: fmt.Sprintf(""+
					"<@&%s> ⭐ Το παίρνουν όσοι έχουν ανεβάσει υλικό για μαθήματα (θέματα, λύσεις θεμάτων κ.λπ.) "+
					"ή είναι γενικότερα ενεργά μέλη της κοινότητας. Μπορούν να κάνουν pin μηνύματα που θεωρούν σημαντικά "+
					"για κάποιο κανάλι ή μάθημα, και να χρησιμοποιούν τα mentions @everyone και @here.\n"+
					"<@&%s> ✅ Έχουν περάσει το στάδιο ταυτοποίησης του ακαδημαϊκού τους λογαριασμού.\n"+
					"<@&%s> 👀 Επιβεβαιωμένοι φοιτητές άλλων τμημάτων ή πανεπιστημίων.",
					goldenRoleID, hmuRoleID, outsiderRoleID)},
				{Name: "Bots", Value: "**Βοτάκια** 🤖 Τα μποτάκια του server μας."},
			},
		},
	}
}

// cmdRulesPost posts the rules and the role list in a channel.
func (h *Handler) cmdRulesPost(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	opt := findOpt(sub.Options, "channel")
	if opt == nil {
		h.reply(s, i, "Διάλεξε κανάλι.")
		return
	}
	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	ch, err := h.channel(ctx, s, opt.ChannelValue(nil).ID)
	if err != nil {
		h.reply(s, i, fmt.Sprintf("Δεν βρήκα το κανάλι: %v", err))
		return
	}
	if !h.inHomeGuild(ch.GuildID) || (ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews) {
		h.reply(s, i, "Διάλεξε ένα κανάλι κειμένου αυτού του server.")
		return
	}
	// One message per embed: they stay readable and can be edited separately.
	for _, e := range rulesEmbeds() {
		if _, err := s.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
			Embeds:          []*discordgo.MessageEmbed{e},
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}, discordgo.WithContext(ctx)); err != nil {
			h.reply(s, i, fmt.Sprintf("Δεν μπόρεσα να στείλω όλα τα μηνύματα στο <#%s>: %v", ch.ID, err))
			return
		}
	}
	h.log.Info("rules: posted", "channel_id", ch.ID, "user_id", actorID(i))
	h.reply(s, i, fmt.Sprintf("✅ Οι κανόνες και οι ρόλοι στάλθηκαν στο <#%s>.", ch.ID))
}
