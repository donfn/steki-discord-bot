package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/bot"
	"steki-bot/internal/config"
	"steki-bot/internal/materials"
	"steki-bot/internal/store"
	"steki-bot/internal/verify"
)

// A member reports a file from the material page. The check runs at once; a
// report that holds pulls the file off the page and the bot posts a card in
// the log channel. An admin then removes the file for good or puts it back.
// When the check cannot run, the file is pulled all the same and left to the
// admins. A report that does not hold gets a silent card, for the record.
// Unlike the cards of /materials rescan, the bucket copy stays until the
// admin decides, so putting a file back is instant.

const (
	reportRemovePrefix  = "matrep:rm:"
	reportRestorePrefix = "matrep:ok:"

	reportCardsPerPass = 10

	// uncheckedNote is the verdict of a report whose check could not run.
	uncheckedNote = "Ο αυτόματος έλεγχος δεν ήταν διαθέσιμος· το αρχείο κρύφτηκε μέχρι να αποφασίσει κάποιος admin."
)

// reportMu keeps two passes from posting the same card.
var reportMu sync.Mutex

// Report implements verify.Reporter.
func (src *materialSource) Report(ctx context.Context, userID, channelID, fileID, reason string) (*verify.ReportResult, error) {
	h := src.h
	m, err := h.st.ChannelMaterial(ctx, channelID, fileID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, verify.ErrNoMaterial
	}
	if err != nil {
		return nil, err
	}

	id, left, err := h.st.ReserveReport(ctx, fileID, userID, reason, time.Now())
	var limit *store.ReportLimitError
	switch {
	case errors.Is(err, store.ErrReportDuplicate):
		return nil, verify.ErrReportAlready
	case errors.As(err, &limit):
		return nil, &verify.ReportLimitError{ResetsAt: limit.ResetsAt}
	case err != nil:
		return nil, err
	}
	// A report that leaves no card is given back.
	drop := func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bot.DBTimeout)
		defer cancel()
		if err := h.st.DropReport(dctx, id); err != nil {
			h.Log.Error("report: give the report back", "report_id", id, "error", err)
		}
	}

	open := func(ctx context.Context) (io.ReadCloser, error) {
		r, _, err := h.mat.Bucket().Open(ctx, m.ObjectKey)
		return r, err
	}
	// Without a verdict the file is pulled anyway and the admins decide.
	v := materials.ReportVerdict{Valid: true, Note: uncheckedNote}
	if h.judge != nil {
		if got, err := h.judge.JudgeReport(ctx, m, open, reason); err != nil {
			h.Log.Warn("report: check failed; leaving it to the admins", "attachment_id", fileID, "error", err)
		} else {
			v = got
		}
	}
	if !v.Valid {
		if err := h.st.SettleReport(ctx, id, store.ReportInvalid, v.Note); err != nil {
			h.Log.Error("report: record verdict", "report_id", id, "error", err)
		}
		h.Log.Info("report: declined", "attachment_id", fileID, "user_id", userID, "note", v.Note)
		go h.Safely("report_cards", func() { h.announceReports(src.s) })
		return &verify.ReportResult{Note: v.Note, Left: left}, nil
	}

	note := "αναφορά από <@" + userID + ">: " + bot.TruncateRunes(reason, 200)
	_, pulled, err := h.st.PullReported(ctx, channelID, fileID, note)
	if err != nil {
		drop()
		return nil, err
	}
	if !pulled {
		// Another report got there first, or an admin took the file down.
		drop()
		return nil, verify.ErrNoMaterial
	}
	if err := h.st.SettleReport(ctx, id, store.ReportValid, v.Note); err != nil {
		h.Log.Error("report: record verdict", "report_id", id, "error", err)
	}
	h.Log.Warn("report: file pulled", "attachment_id", fileID, "user_id", userID, "note", v.Note)
	h.refreshSticky(src.s, m.HomeChannelID)
	go h.Safely("report_cards", func() { h.announceReports(src.s) })
	return &verify.ReportResult{Pulled: true, Note: v.Note, Left: left}, nil
}

// reportCard is the log-channel message for one report. A pulled file gets
// the admin's buttons; a declined report is only for the record.
func reportCard(m *store.Material, r *store.Report, imageURL string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	desc := "Ο αυτόματος έλεγχος έκρινε την αναφορά βάσιμη, οπότε το αρχείο κρύφτηκε από τη σελίδα υλικού. Το αντίγραφο στο bucket μένει μέχρι να αποφασίσει κάποιος admin."
	color := 0xE67E22
	switch {
	case r.Status == store.ReportInvalid:
		desc = "Ο αυτόματος έλεγχος δεν βρήκε πρόβλημα, οπότε το αρχείο μένει στη σελίδα. Δεν χρειάζεται κάτι από εσάς, εκτός αν διαφωνείτε."
		color = 0x95A5A6
	case r.Note == uncheckedNote:
		desc = "Ο αυτόματος έλεγχος δεν ήταν διαθέσιμος, οπότε το αρχείο κρύφτηκε από τη σελίδα υλικού χωρίς έλεγχο. Το αντίγραφο στο bucket μένει μέχρι να αποφασίσει κάποιος admin."
	}
	e := &discordgo.MessageEmbed{
		Title:       "🚩 Αναφορά υλικού",
		Description: desc,
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Λόγος", Value: orDash(bot.TruncateRunes(r.Reason, 600))},
			{Name: "Έλεγχος", Value: orDash(r.Note)},
			{Name: "Αναφέρθηκε από", Value: "<@" + r.ReporterID + ">", Inline: true},
			{Name: "Ανέβηκε από", Value: "<@" + m.UploaderID + ">", Inline: true},
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
	if r.Status == store.ReportInvalid {
		return e, nil
	}
	return e, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Αφαίρεση οριστικά", Style: discordgo.DangerButton, CustomID: reportRemovePrefix + m.AttachmentID},
		discordgo.Button{Label: "Επαναφορά στη σελίδα", Style: discordgo.SuccessButton, CustomID: reportRestorePrefix + m.AttachmentID},
	}}}
}

// announceReports posts a card for every valid report that has none and
// returns how many it posted. A report being filed starts it; a card that
// failed to post, or one lost to a restart, waits for /materials cards.
func (h *Handler) announceReports(s *discordgo.Session) (posted int) {
	if h.mat == nil {
		return 0
	}
	reportMu.Lock()
	defer reportMu.Unlock()

	for {
		n, more := h.announceReportsPage(s)
		if posted += n; !more {
			return posted
		}
	}
}

// announceReportsPage posts the cards of one page of reports. more reports
// whether a full page went out, so another may be waiting.
func (h *Handler) announceReportsPage(s *discordgo.Session) (posted int, more bool) {
	ctx, cancel := h.CtxTimeout(bot.ReapTimeout)
	defer cancel()
	rows, err := h.st.ReportsToAnnounce(ctx, reportCardsPerPass)
	if err != nil {
		h.Log.Error("report: list reports to announce", "error", err)
		return 0, false
	}
	refresh := h.refreshMaterialSource(s)
	skipped := 0
	for _, r := range rows {
		m, err := h.st.MaterialByID(ctx, r.AttachmentID)
		if err != nil {
			h.Log.Error("report: load reported file", "attachment_id", r.AttachmentID, "error", err)
			skipped++
			continue
		}
		url := ""
		if m.ContentType == materials.TypeJPEG {
			// The stored link may have expired; the card shows the picture
			// from a fresh one, or without it when the message is gone.
			if url, err = refresh(ctx, m); err != nil {
				if !errors.Is(err, materials.ErrSourceGone) {
					h.Log.Warn("report: refresh image link", "attachment_id", m.AttachmentID, "error", err)
				}
				url = ""
			}
		}
		embed, components := reportCard(m, r, url)
		send := &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed}, Components: components,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}
		if components == nil {
			// Nothing to decide: the card is posted without a notification.
			send.Flags = discordgo.MessageFlagsSuppressNotifications
		}
		msg, err := s.ChannelMessageSendComplex(config.LogChannelID, send, discordgo.WithContext(ctx))
		if err != nil {
			h.Log.Error("report: post card", "attachment_id", r.AttachmentID, "error", err)
			time.AfterFunc(reviewRetry, func() {
				if h.Base.Err() == nil {
					h.Safely("report_cards", func() { h.announceReports(s) })
				}
			})
			return posted, false
		}
		posted++
		if err := h.st.SetReportCard(ctx, r.ID, msg.ID); err != nil {
			// Not recorded, so the report would be listed again: stop rather
			// than post its card twice.
			h.Log.Error("report: record card", "report_id", r.ID, "error", err)
			return posted, false
		}
	}
	// A report that could not be loaded is listed again and again, so a page
	// that held one is the last.
	return posted, len(rows) == reportCardsPerPass && skipped == 0
}

// onReportButton applies an admin's decision on a reported file.
func (h *Handler) onReportButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	id := i.MessageComponentData().CustomID
	remove := strings.HasPrefix(id, reportRemovePrefix)
	attachmentID := strings.TrimPrefix(strings.TrimPrefix(id, reportRemovePrefix), reportRestorePrefix)

	if h.mat == nil || !h.InHomeGuild(i.GuildID) || i.Member == nil || !bot.HasAnyRole(i.Member, []string{bot.StaffRoleID, bot.ModRoleID}) {
		h.RespondNow(s, i, "⛔ Μόνο η Ομάδα Διαχείρισης μπορεί να αποφασίσει.")
		return
	}
	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()
	who := bot.ActorID(i)
	note := "επαναφέρθηκε από <@" + who + ">"
	if remove {
		note = "αφαιρέθηκε από <@" + who + ">"
	}
	m, won, err := h.st.ResolveReport(ctx, attachmentID, remove, note)
	if err != nil {
		h.Log.Error("report: resolve", "attachment_id", attachmentID, "error", err)
		h.RespondNow(s, i, "Δεν μπόρεσα να αποθηκεύσω την απόφαση. Δοκίμασε ξανά.")
		return
	}
	if !won {
		h.RespondNow(s, i, "Η απόφαση έχει ήδη παρθεί: "+orDash(derefStr(m.ScreenNote)))
		return
	}

	// Removing is for good: the bucket copy goes too. The decision is already
	// recorded, so a failure here is for an admin to finish with
	// /materials remove.
	bucketErr := error(nil)
	if remove {
		bucketErr = h.mat.Bucket().Delete(ctx, m.ObjectKey)
		if bucketErr != nil {
			h.Log.Error("report: remove from bucket", "attachment_id", attachmentID, "error", bucketErr)
		}
	}

	embed := &discordgo.MessageEmbed{Title: "🚩 Αναφορά υλικού"}
	if len(i.Message.Embeds) > 0 {
		cp := *i.Message.Embeds[0]
		embed = &cp
	}
	if remove {
		embed.Color = 0xE74C3C
		embed.Description = "🗑️ Αφαιρέθηκε οριστικά από <@" + who + ">."
		if bucketErr != nil {
			embed.Description += " Το αντίγραφο στο bucket δεν σβήστηκε: τρέξε `/materials remove` για το μήνυμα."
		}
	} else {
		embed.Color = 0x2ECC71
		embed.Description = "✅ Επαναφέρθηκε στη σελίδα υλικού από <@" + who + ">."
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{},
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	}); err != nil {
		h.Log.Warn("report: update card", "error", err)
	}
	h.Log.Info("report: decided", "attachment_id", attachmentID, "removed", remove, "user_id", who)
	h.refreshSticky(s, m.HomeChannelID)
}
