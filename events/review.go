package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
	"steki-bot/materials"
	"steki-bot/store"
)

// Images the content check flags are not listed and not backed up. The bot
// posts a card for each in the log channel, and an admin decides with its two
// buttons: approving sends the file on to the bucket and the page, rejecting
// keeps it out for good.

const (
	reviewApprovePrefix = "matrev:ok:"
	reviewRejectPrefix  = "matrev:no:"

	// reviewCardsPerPass bounds how many cards one pass posts, so a rescan that
	// flags hundreds of images does not flood the channel at once.
	reviewCardsPerPass = 10

	// reviewEvery is how often cards the worker's hook missed are posted.
	reviewEvery = 5 * time.Minute
)

// reviewMu keeps two passes from posting the same card.
var reviewMu sync.Mutex

// reviewCard is the log-channel message for one held file.
func reviewCard(m *store.Material, imageURL string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	e := &discordgo.MessageEmbed{
		Title:       "🖼️ Εικόνα για έλεγχο",
		Description: "Δεν ανέβηκε στο bucket και δεν φαίνεται στη σελίδα υλικού μέχρι να αποφασίσει κάποιος admin.",
		Color:       0xE67E22,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Γιατί", Value: orDash(derefStr(m.ScreenNote))},
			{Name: "Μέλος", Value: "<@" + m.UploaderID + ">", Inline: true},
			{Name: "Κανάλι", Value: "<#" + m.ChannelID + ">", Inline: true},
			{Name: "Μήνυμα", Value: fmt.Sprintf("[άνοιγμα](https://discord.com/channels/%s/%s/%s)", m.GuildID, m.ChannelID, m.MessageID), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: m.Filename},
	}
	if m.Caption != "" {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Κείμενο", Value: truncateRunes(m.Caption, 300)})
	}
	if imageURL != "" {
		e.Image = &discordgo.MessageEmbedImage{URL: imageURL}
	}
	return e, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Έγκριση και ανέβασμα", Style: discordgo.SuccessButton, CustomID: reviewApprovePrefix + m.AttachmentID},
		discordgo.Button{Label: "Απόρριψη", Style: discordgo.DangerButton, CustomID: reviewRejectPrefix + m.AttachmentID},
	}}}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// announceReviews posts a card for every held file that has none.
func (h *Handler) announceReviews(s *discordgo.Session) {
	if h.mat == nil || !h.mat.Vision() {
		return
	}
	reviewMu.Lock()
	defer reviewMu.Unlock()

	ctx, cancel := h.ctxTimeout(reapTimeout)
	defer cancel()
	rows, err := h.st.ReviewsToAnnounce(ctx, reviewCardsPerPass)
	if err != nil {
		h.log.Error("review: list held files", "error", err)
		return
	}
	refresh := h.refreshMaterialSource(s)
	for _, m := range rows {
		// The stored link may have expired; the card shows the picture from a
		// fresh one, or without it when the message is gone.
		url, err := refresh(ctx, m)
		if err != nil && !errors.Is(err, materials.ErrSourceGone) {
			h.log.Warn("review: refresh image link", "attachment_id", m.AttachmentID, "error", err)
			url = m.SourceURL
		}
		embed, components := reviewCard(m, url)
		msg, err := s.ChannelMessageSendComplex(config.LogChannelID, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed}, Components: components,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}, discordgo.WithContext(ctx))
		if err != nil {
			h.log.Error("review: post card", "attachment_id", m.AttachmentID, "error", err)
			return
		}
		if err := h.st.SetReviewMessage(ctx, m.AttachmentID, msg.ID); err != nil {
			h.log.Error("review: record card", "attachment_id", m.AttachmentID, "error", err)
		}
	}
	if len(rows) == reviewCardsPerPass {
		// More are waiting: the next pass takes them.
		go h.safely("review_cards", func() { h.announceReviews(s) })
	}
}

// onReviewButton applies an admin's decision.
func (h *Handler) onReviewButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	id := i.MessageComponentData().CustomID
	approve := strings.HasPrefix(id, reviewApprovePrefix)
	attachmentID := strings.TrimPrefix(strings.TrimPrefix(id, reviewApprovePrefix), reviewRejectPrefix)

	if h.mat == nil || !h.inHomeGuild(i.GuildID) || i.Member == nil || !hasAnyRole(i.Member, []string{staffRoleID, modRoleID}) {
		h.respondNow(s, i, "⛔ Μόνο η Ομάδα Διαχείρισης μπορεί να αποφασίσει.")
		return
	}
	ctx, cancel := h.ctxTimeout(dbTimeout)
	defer cancel()
	who := actorID(i)
	note := "απορρίφθηκε από <@" + who + ">"
	if approve {
		note = "εγκρίθηκε από <@" + who + ">"
	}
	m, won, err := h.st.ResolveReview(ctx, attachmentID, approve, note)
	if err != nil {
		h.log.Error("review: resolve", "attachment_id", attachmentID, "error", err)
		h.respondNow(s, i, "Δεν μπόρεσα να αποθηκεύσω την απόφαση. Δοκίμασε ξανά.")
		return
	}
	if !won {
		h.respondNow(s, i, "Η απόφαση έχει ήδη παρθεί: "+orDash(derefStr(m.ScreenNote)))
		return
	}

	// Rewrite the card in place: the decision, and no buttons.
	embed := &discordgo.MessageEmbed{Title: "🖼️ Εικόνα για έλεγχο"}
	if len(i.Message.Embeds) > 0 {
		cp := *i.Message.Embeds[0]
		embed = &cp
	}
	if approve {
		embed.Color = 0x2ECC71
		embed.Description = "✅ Εγκρίθηκε από <@" + who + ">. Ανεβαίνει στο bucket και στη σελίδα υλικού."
	} else {
		embed.Color = 0xE74C3C
		embed.Description = "🚫 Απορρίφθηκε από <@" + who + ">. Δεν ανέβηκε και δεν θα ανέβει."
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{},
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	}); err != nil {
		h.log.Warn("review: update card", "error", err)
	}
	h.log.Info("review: decided", "attachment_id", attachmentID, "approved", approve, "user_id", who)
	if approve {
		h.mat.Wake()
	}
}

// cmdMaterialsRescan queues the images uploaded before the content check
// existed.
func (h *Handler) cmdMaterialsRescan(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	if h.mat == nil || !h.mat.Vision() {
		h.reply(s, i, "Ο έλεγχος εικόνων είναι ανενεργός: χρειάζεται το `GOOGLE_AI_TOKEN`.")
		return
	}
	ctx, cancel := context.WithTimeout(h.base, dbTimeout)
	defer cancel()
	n, err := h.st.CountUnchecked(ctx)
	if err != nil {
		h.reply(s, i, fmt.Sprintf("Δεν μπόρεσα να μετρήσω τις εικόνες: %v", err))
		return
	}
	if n == 0 {
		h.reply(s, i, "✅ Όλες οι εικόνες έχουν ήδη ελεγχθεί.")
		return
	}
	h.mat.Rescan()
	h.log.Info("materials: rescan started", "images", n, "user_id", actorID(i))
	h.reply(s, i, fmt.Sprintf("🔎 Ξεκίνησε ο έλεγχος για **%d** εικόνες. Όσες δεν είναι υλικό μαθήματος ή δείχνουν ανθρώπους "+
		"βγαίνουν από τη σελίδα και το bucket και έρχονται εδώ για έγκριση. Αν τελειώσει το quota του Gemini, "+
		"ο έλεγχος περιμένει και συνεχίζει μόνος του. Αν γίνει επανεκκίνηση πριν τελειώσει, τρέξε ξανά την εντολή.", n))
}
