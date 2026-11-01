package moderation

import (
	"fmt"

	"steki-bot/bot"

	"github.com/bwmarrin/discordgo"
)

// rulesEmbeds are the server rules, the note about pinned messages and the
// role list, as one embed each so they read as separate messages.
const rulesColor = 0xFFC900

func rulesEmbeds() []*discordgo.MessageEmbed {
	return []*discordgo.MessageEmbed{
		{
			Title:       "Κανόνες του server",
			Description: "Ο server λειτουργεί σαν δημόσιο φόρουμ. Μην υποθέτεις ότι υπάρχει ανωνυμία ή ιδιωτικότητα στις συζητήσεις.",
			Color:       rulesColor,
			Fields: []*discordgo.MessageEmbedField{
				{Name: "Τι ισχύει", Value: "" +
					"• Όχι spam.\n" +
					"• Για περιεχόμενο στα όρια του ακραίου αποφασίζουν οι admins. Μη γράφεις πράγματα που μπορεί να εκθέσουν εσένα ή τους διαχειριστές.\n" +
					"• Απαγορεύονται υπαινιγμοί, προσβολές και ονοματολογία για καθηγητές, φοιτητές ή άλλα πρόσωπα.\n" +
					"• Δεν είμαστε υποχρεωμένοι να σε κρατήσουμε στον server. Αν κρίνουμε ότι είχες ή θα έχεις παραβατική συμπεριφορά, δεν θα σου επιτραπεί η πρόσβαση."},
				{Name: "Αν παραβιάσεις τους κανόνες", Value: "" +
					"Μπαίνεις σε προσωρινό mute: μπορείς να διαβάζεις αλλά όχι να γράφεις, και τα μηνύματά σου διαγράφονται. " +
					"Αν συνεχίσεις, αφαιρείσαι από τον server."},
				{Name: "Quiz και εξετάσεις", Value: "" +
					"Αν ανεβάζεις quiz, π.χ. από το eclass, να μη φαίνονται οι απαντήσεις. " +
					"Κάλυψέ τες στο screenshot πριν το ανεβάσεις."},
			},
		},
		{
			Title: "Διαχείριση",
			Description: "Θέλουμε ο server να συνεχίσει και αφού τελειώσουμε με το ΕΛΜΕΠΑ. " +
				"Αν σε ενδιαφέρει να γίνεις admin ή mod, στείλε μας μήνυμα.",
			Color: rulesColor,
		},
		{
			Title:       "📌 Υλικό στα pinned",
			Description: "Στα pinned μηνύματα κάθε καναλιού υπάρχει χρήσιμο υλικό για το αντίστοιχο μάθημα.",
			Color:       rulesColor,
		},
		{
			Title: "Ρόλοι",
			Color: rulesColor,
			Fields: []*discordgo.MessageEmbedField{
				{Name: "Διαχείριση", Value: fmt.Sprintf(""+
					"<@&%s> Οι διαχειριστές του server, με πλήρη δικαιώματα.\n"+
					"<@&%s> Διαγράφουν μηνύματα, δημιουργούν και επεξεργάζονται κανάλια, και κάνουν ban ή time-out σε χρήστες.",
					bot.StaffRoleID, bot.ModRoleID)},
				{Name: "Μέλη", Value: fmt.Sprintf(""+
					"<@&%s> ⭐ Το παίρνουν όσοι έχουν ανεβάσει υλικό για μαθήματα (θέματα, λύσεις θεμάτων κ.λπ.) "+
					"ή είναι γενικά ενεργά μέλη της κοινότητας. Μπορούν να κάνουν pin μηνύματα που θεωρούν σημαντικά "+
					"για κάποιο κανάλι ή μάθημα, και να χρησιμοποιούν τα mentions @everyone και @here.\n"+
					"<@&%s> ✅ Έχουν επιβεβαιώσει τον ακαδημαϊκό τους λογαριασμό.\n"+
					"<@&%s> 👀 Επιβεβαιωμένοι φοιτητές άλλων τμημάτων ή πανεπιστημίων.",
					bot.GoldenRoleID, bot.HMURoleID, bot.OutsiderRoleID)},
				{Name: "Bots", Value: "**Βοτάκια** 🤖 Τα bots του server."},
			},
		},
	}
}

// cmdRulesPost posts the rules and the role list in a channel.
func (h *Handler) cmdRulesPost(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	opt := bot.FindOpt(sub.Options, "channel")
	if opt == nil {
		h.Reply(s, i, "Διάλεξε κανάλι.")
		return
	}
	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	ch, err := h.Channel(ctx, s, opt.ChannelValue(nil).ID)
	if err != nil {
		h.Reply(s, i, fmt.Sprintf("Δεν βρήκα το κανάλι: %v", err))
		return
	}
	if !h.InHomeGuild(ch.GuildID) || (ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews) {
		h.Reply(s, i, "Διάλεξε ένα κανάλι κειμένου αυτού του server.")
		return
	}
	// One message per embed: they stay readable and can be edited separately.
	for _, e := range rulesEmbeds() {
		if _, err := s.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
			Embeds:          []*discordgo.MessageEmbed{e},
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}, discordgo.WithContext(ctx)); err != nil {
			h.Reply(s, i, fmt.Sprintf("Δεν μπόρεσα να στείλω όλα τα μηνύματα στο <#%s>: %v", ch.ID, err))
			return
		}
	}
	h.Log.Info("rules: posted", "channel_id", ch.ID, "user_id", bot.ActorID(i))
	h.Reply(s, i, fmt.Sprintf("✅ Οι κανόνες και οι ρόλοι στάλθηκαν στο <#%s>.", ch.ID))
}
