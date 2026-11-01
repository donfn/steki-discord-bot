package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
	"steki-bot/config"
	"steki-bot/dc"
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

	// reviewRetry is how long to wait before posting cards again after a
	// failure. Cards are otherwise posted when a file is held.
	reviewRetry = 5 * time.Minute
)

// reviewMu keeps two passes from posting the same card.
var reviewMu sync.Mutex

// reviewCard is the log-channel message for one held file.
func reviewCard(m *store.Material, imageURL string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	e := &discordgo.MessageEmbed{
		Title:       "🖼️ Εικόνα για έλεγχο",
		Description: "Δεν έχει ανέβει στο bucket και δεν φαίνεται στη σελίδα υλικού μέχρι να αποφασίσει κάποιος admin.",
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
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Κείμενο", Value: bot.TruncateRunes(m.Caption, 300)})
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

// courseChannelIDs lists the course channels, the only ones whose images the
// content check looks at: memes in the general channels are none of its
// business.
func (h *Handler) courseChannelIDs(ctx context.Context, s *discordgo.Session) ([]string, error) {
	channels, err := dc.GuildChannels(ctx, s, h.Log, h.GuildID())
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ch := range channels {
		if isCourseChannel(ch) {
			ids = append(ids, ch.ID)
		}
	}
	return ids, nil
}

// announceReviews posts a card for every held file that has none and returns
// how many it posted. A file being held starts it; a card that failed to post,
// or one lost to a restart, waits for /materials cards.
func (h *Handler) announceReviews(s *discordgo.Session) (posted int) {
	if h.mat == nil || !h.mat.Vision() {
		return 0
	}
	reviewMu.Lock()
	defer reviewMu.Unlock()

	for {
		n, more := h.announceReviewsPage(s)
		if posted += n; !more {
			return posted
		}
	}
}

// announceReviewsPage posts the cards of one page of held files. more reports
// whether a full page went out, so another may be waiting.
func (h *Handler) announceReviewsPage(s *discordgo.Session) (posted int, more bool) {
	ctx, cancel := h.CtxTimeout(bot.ReapTimeout)
	defer cancel()
	rows, err := h.st.ReviewsToAnnounce(ctx, reviewCardsPerPass)
	if err != nil {
		h.Log.Error("review: list held files", "error", err)
		return 0, false
	}
	refresh := h.refreshMaterialSource(s)
	for _, m := range rows {
		// The stored link may have expired; the card shows the picture from a
		// fresh one, or without it when the message is gone.
		url, err := refresh(ctx, m)
		if err != nil && !errors.Is(err, materials.ErrSourceGone) {
			h.Log.Warn("review: refresh image link", "attachment_id", m.AttachmentID, "error", err)
			url = m.SourceURL
		}
		embed, components := reviewCard(m, url)
		msg, err := s.ChannelMessageSendComplex(config.LogChannelID, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed}, Components: components,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}, discordgo.WithContext(ctx))
		if err != nil {
			h.Log.Error("review: post card", "attachment_id", m.AttachmentID, "error", err)
			time.AfterFunc(reviewRetry, func() {
				if h.Base.Err() == nil {
					h.Safely("review_cards", func() { h.announceReviews(s) })
				}
			})
			return posted, false
		}
		posted++
		if err := h.st.SetReviewMessage(ctx, m.AttachmentID, msg.ID); err != nil {
			// Not recorded, so the file would be listed again: stop rather than
			// post its card twice.
			h.Log.Error("review: record card", "attachment_id", m.AttachmentID, "error", err)
			return posted, false
		}
	}
	return posted, len(rows) == reviewCardsPerPass
}

// onReviewButton applies an admin's decision.
func (h *Handler) onReviewButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	id := i.MessageComponentData().CustomID
	approve := strings.HasPrefix(id, reviewApprovePrefix)
	attachmentID := strings.TrimPrefix(strings.TrimPrefix(id, reviewApprovePrefix), reviewRejectPrefix)

	if h.mat == nil || !h.InHomeGuild(i.GuildID) || i.Member == nil || !bot.HasAnyRole(i.Member, []string{bot.StaffRoleID, bot.ModRoleID}) {
		h.RespondNow(s, i, "⛔ Μόνο η Ομάδα Διαχείρισης μπορεί να αποφασίσει.")
		return
	}
	ctx, cancel := h.CtxTimeout(bot.DBTimeout)
	defer cancel()
	who := bot.ActorID(i)
	note := "απορρίφθηκε από <@" + who + ">"
	if approve {
		note = "εγκρίθηκε από <@" + who + ">"
	}
	m, won, err := h.st.ResolveReview(ctx, attachmentID, approve, note)
	if err != nil {
		h.Log.Error("review: resolve", "attachment_id", attachmentID, "error", err)
		h.RespondNow(s, i, "Δεν μπόρεσα να αποθηκεύσω την απόφαση. Δοκίμασε ξανά.")
		return
	}
	if !won {
		h.RespondNow(s, i, "Η απόφαση έχει ήδη παρθεί: "+orDash(derefStr(m.ScreenNote)))
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
		embed.Description = "✅ Εγκρίθηκε από <@" + who + ">. Ανεβαίνει τώρα στο bucket και στη σελίδα υλικού."
	} else {
		embed.Color = 0xE74C3C
		embed.Description = "🚫 Απορρίφθηκε από <@" + who + ">. Δεν θα ανέβει."
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{},
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	}); err != nil {
		h.Log.Warn("review: update card", "error", err)
	}
	h.Log.Info("review: decided", "attachment_id", attachmentID, "approved", approve, "user_id", who)
	if approve {
		h.mat.Wake()
	}
}

// cmdMaterialsRescan queues the images uploaded before the content check
// existed.
func (h *Handler) cmdMaterialsRescan(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	if h.mat == nil || !h.mat.Vision() {
		h.Reply(s, i, "Ο έλεγχος εικόνων είναι ανενεργός: χρειάζεται το `GOOGLE_AI_TOKEN`.")
		return
	}
	ctx, cancel := context.WithTimeout(h.Base, bot.DBTimeout)
	defer cancel()
	n, err := h.mat.Unchecked(ctx)
	if err != nil {
		h.Reply(s, i, fmt.Sprintf("Δεν μπόρεσα να μετρήσω τις εικόνες: %v", err))
		return
	}
	if n == 0 {
		h.Reply(s, i, "✅ Όλες οι εικόνες έχουν ήδη ελεγχθεί.")
		return
	}
	h.mat.Rescan()
	h.Log.Info("materials: rescan started", "images", n, "user_id", bot.ActorID(i))
	h.Reply(s, i, fmt.Sprintf("🔎 Ξεκίνησε ο έλεγχος για **%d** εικόνες. Όσες δεν είναι υλικό μαθήματος ή δείχνουν ανθρώπους "+
		"αφαιρούνται από τη σελίδα και το bucket και έρχονται εδώ για έγκριση. Αν τελειώσει το quota του Gemini, "+
		"ο έλεγχος περιμένει και συνεχίζει μόνος του. Αν το bot επανεκκινηθεί πριν τελειώσει, τρέξε ξανά την εντολή.", n))
}

// cmdMaterialsCompress compresses the files stored before compression existed.
// It can be rerun at any time: files already compressed are skipped.
func (h *Handler) cmdMaterialsCompress(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	if h.mat == nil {
		h.Reply(s, i, "Το bucket είναι ανενεργό.")
		return
	}
	if !h.compressing.CompareAndSwap(false, true) {
		h.Reply(s, i, "⏳ Η συμπίεση τρέχει ήδη. Το αποτέλεσμα θα έρθει στο κανάλι των logs.")
		return
	}
	h.Log.Info("materials: compression started", "user_id", bot.ActorID(i))
	h.Reply(s, i, "🗜️ Ξεκίνησε η συμπίεση των αρχείων του bucket. Το αποτέλεσμα θα έρθει στο κανάλι των logs. "+
		"Αν το bot επανεκκινηθεί πριν τελειώσει, τρέξε ξανά την εντολή.")
	go h.Safely("materials_compress", func() {
		defer h.compressing.Store(false)
		r := h.mat.CompressAll(h.Base)
		h.Log.Info("materials: compression finished", "result", fmt.Sprintf("%+v", r))
		h.Modlog.Post(s, fmt.Sprintf("🗜️ Συμπίεση bucket: **%d** νέα, **%d** ήδη συμπιεσμένα, **%d** λείπουν από το bucket, **%d** απέτυχαν. "+
			"Τα νέα πήγαν από %s σε %s.", r.Compressed, r.Already, r.Missing, r.Failed, formatBytes(r.Before), formatBytes(r.After)))
	})
}

// messageIDFrom reads a message ID from a Discord message link or a bare ID.
func messageIDFrom(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if i := strings.LastIndex(v, "/"); i >= 0 {
		v = v[i+1:]
	}
	v = strings.SplitN(v, "?", 2)[0]
	return v, isSnowflake(v)
}

func isSnowflake(v string) bool {
	if len(v) < 15 || len(v) > 21 {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// cmdMaterialsRemove takes the material of a message off the pages and out of
// the bucket for good, whatever its state.
func (h *Handler) cmdMaterialsRemove(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	if h.mat == nil {
		h.Reply(s, i, "Το υλικό είναι απενεργοποιημένο: δεν έχει ρυθμιστεί bucket.")
		return
	}
	msgOpt := bot.FindOpt(sub.Options, "message")
	if msgOpt == nil {
		h.Reply(s, i, "Δώσε το link ή το ID του μηνύματος.")
		return
	}
	messageID, ok := messageIDFrom(msgOpt.StringValue())
	if !ok {
		h.Reply(s, i, "Δεν βρήκα αυτό το μήνυμα. Δώσε το link του (δεξί κλικ → Copy Message Link) ή το ID του.")
		return
	}
	filter := ""
	if f := bot.FindOpt(sub.Options, "file"); f != nil {
		filter = strings.ToLower(strings.TrimSpace(f.StringValue()))
	}
	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	rows, err := h.st.MaterialsOfMessage(ctx, messageID)
	if err != nil {
		h.Reply(s, i, fmt.Sprintf("Η αναζήτηση απέτυχε: %v", err))
		return
	}
	var removed, skipped []string
	homes := map[string]bool{}
	for _, m := range rows {
		if filter != "" && !strings.Contains(strings.ToLower(m.Filename), filter) {
			continue
		}
		if m.Status == store.MaterialRejected && m.BackedUpAt == nil {
			skipped = append(skipped, m.Filename)
			continue
		}
		// The bucket copy goes first: if that fails nothing changes and the
		// command can be run again.
		if err := h.mat.Bucket().Delete(ctx, m.ObjectKey); err != nil {
			h.Log.Error("materials: remove from bucket", "attachment_id", m.AttachmentID, "error", err)
			h.Reply(s, i, fmt.Sprintf("Δεν μπόρεσα να σβήσω το `%s` από το bucket: %v", m.Filename, err))
			return
		}
		if err := h.st.RemoveMaterial(ctx, m.AttachmentID, "αφαιρέθηκε από <@"+bot.ActorID(i)+">"); err != nil {
			h.Log.Error("materials: remove", "attachment_id", m.AttachmentID, "error", err)
			h.Reply(s, i, fmt.Sprintf("Σβήστηκε από το bucket αλλά όχι από τη βάση (`%s`): %v. Τρέξε ξανά την εντολή.", m.Filename, err))
			return
		}
		removed = append(removed, m.Filename)
		homes[m.HomeChannelID] = true
	}
	for home := range homes {
		h.refreshSticky(s, home)
	}
	h.Log.Info("materials: removed by admin", "message_id", messageID, "files", len(removed), "user_id", bot.ActorID(i))

	switch {
	case len(removed) > 0:
		msg := "🗑️ Αφαιρέθηκαν από τη σελίδα και το bucket: " + "`" + strings.Join(removed, "`, `") + "`."
		if len(skipped) > 0 {
			msg += " Είχαν ήδη αφαιρεθεί: `" + strings.Join(skipped, "`, `") + "`."
		}
		h.Reply(s, i, msg+"\nΤο μήνυμα στο Discord έμεινε όπως ήταν.")
	case len(skipped) > 0:
		h.Reply(s, i, "Το υλικό αυτού του μηνύματος έχει ήδη αφαιρεθεί.")
	default:
		h.Reply(s, i, "Δεν βρήκα υλικό για αυτό το μήνυμα"+map[bool]string{true: " με αυτό το όνομα αρχείου", false: ""}[filter != ""]+".")
	}
}

// cmdMaterialsScan reads the history of the course channels for material the
// bot has not seen, and places the stickies. It is the only way old messages
// are picked up.
func (h *Handler) cmdMaterialsScan(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	if h.mat == nil || !h.matLive.Load() {
		h.Reply(s, i, "Το υλικό είναι απενεργοποιημένο: δεν έχει ρυθμιστεί bucket ή δεν απαντάει.")
		return
	}
	end, ok := h.Jobs.Begin("scan")
	if !ok {
		h.Reply(s, i, "Ο έλεγχος τρέχει ήδη. Θα γράψω το αποτέλεσμα στο κανάλι των logs όταν τελειώσει.")
		return
	}
	h.Log.Info("materials: scan requested", "user_id", bot.ActorID(i))
	h.Reply(s, i, "🔎 Ξεκίνησε ο έλεγχος των καναλιών μαθημάτων. Μπορεί να κρατήσει αρκετά λεπτά· θα γράψω το αποτέλεσμα στο κανάλι των logs.")
	go h.Safely("materials_scan", func() {
		defer end()
		channels, found := h.scanMaterials(s)
		h.placeStickies(s)
		h.mat.Wake()
		h.AlertMods(s, "✅ Ο έλεγχος των καναλιών μαθημάτων τελείωσε: διαβάστηκαν %d κανάλια και νήματα, βρέθηκαν %d νέα αρχεία.", channels, found)
	})
}

// cmdMaterialsCards posts the cards in the log channel that the admins have not
// been given yet: held images and reports whose card failed to post, or was lost
// to a restart before it did. Cards are sent as files are held and reports are
// filed; nothing looks for the ones that were missed until this runs.
func (h *Handler) cmdMaterialsCards(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	if h.mat == nil {
		h.Reply(s, i, "Το υλικό είναι απενεργοποιημένο: δεν έχει ρυθμιστεί bucket.")
		return
	}
	end, ok := h.Jobs.Begin("cards")
	if !ok {
		h.Reply(s, i, "Η αποστολή καρτών τρέχει ήδη.")
		return
	}
	defer end()
	h.Log.Info("materials: cards requested", "user_id", bot.ActorID(i))

	reviews := h.announceReviews(s)
	reports := h.announceReports(s)
	if reviews+reports == 0 {
		h.Reply(s, i, "✅ Δεν λείπει καμία κάρτα.")
		return
	}
	h.Reply(s, i, fmt.Sprintf("✅ Στάλθηκαν %d κάρτες εικόνων και %d κάρτες αναφορών στο <#%s>.",
		reviews, reports, config.LogChannelID))
}
