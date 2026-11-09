package web

import (
	"errors"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/bot"
	"steki-bot/internal/config"
	"steki-bot/internal/dc"
	"steki-bot/internal/materials"
	"steki-bot/internal/store"
)

// A member who shares a photo of final exam exercises gets bot.ExamRoleID. The
// content check decides; ordinary study material never earns it.
//
// Every award is reported in the log channel with a button that takes the role
// back. When the check was sure the report is silent; when it only thought the
// picture might be an exam it is a normal message, so an admin looks at it.
// Pictures that are plainly not exams cost the admins nothing: no message.

const examUndoPrefix = "matexam:undo:"

// examMu keeps two images of one member from both being rewarded: the second
// finds the role the first granted.
var examMu sync.Mutex

// examCard is the log-channel message for one award.
func examCard(m *store.Material, v materials.Verdict, imageURL string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	desc, color := "Το Gemini είναι σίγουρο ότι η εικόνα δείχνει θέματα τελικών εξετάσεων.", 0x2ECC71
	if v.Exam != materials.ExamSure {
		desc, color = "Το Gemini **δεν είναι σίγουρο** ότι η εικόνα δείχνει θέματα τελικών εξετάσεων. Ο ρόλος δόθηκε· αν δεν ισχύει, ανάκλησέ τον.", 0xE67E22
	}
	e := &discordgo.MessageEmbed{
		Title:       "🎓 Ρόλος για θέματα εξετάσεων",
		Description: desc,
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Μέλος", Value: "<@" + m.UploaderID + ">", Inline: true},
			{Name: "Κανάλι", Value: "<#" + m.ChannelID + ">", Inline: true},
			{Name: "Μήνυμα", Value: "[άνοιγμα](https://discord.com/channels/" + m.GuildID + "/" + m.ChannelID + "/" + m.MessageID + ")", Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: m.Filename},
	}
	if v.Note != "" {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Τι είδε", Value: bot.TruncateRunes(v.Note, 300)})
	}
	if imageURL != "" {
		e.Image = &discordgo.MessageEmbedImage{URL: imageURL}
	}
	return e, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Ανάκληση ρόλου", Style: discordgo.DangerButton, CustomID: examUndoPrefix + m.UploaderID},
	}}}
}

// awardExamRole gives the uploader of an exam photo the role and reports it. A
// member who already has the role, or has left, is left alone and nothing is
// posted.
func (h *Handler) awardExamRole(s *discordgo.Session, m *store.Material, v materials.Verdict) {
	examMu.Lock()
	defer examMu.Unlock()
	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()
	log := h.Log.With("attachment_id", m.AttachmentID, "user_id", m.UploaderID)

	// Asked of Discord, not the cache, which may not yet show a role granted a
	// moment ago for the previous image.
	member, err := s.GuildMember(m.GuildID, m.UploaderID, discordgo.WithContext(ctx))
	switch {
	case dc.IsMissingResource(err):
		return // left the server
	case err != nil:
		log.Error("exam: look up member", "error", err)
		return
	case bot.HasRole(member, bot.ExamRoleID):
		return
	}
	if err := h.GrantRole(ctx, s, m.GuildID, m.UploaderID, bot.ExamRoleID); err != nil {
		log.Error("exam: grant role", "error", err)
		h.AlertMods(s, "⚠️ Δεν μπόρεσα να δώσω τον ρόλο θεμάτων εξετάσεων στο <@%s> (%v).", m.UploaderID, err)
		return
	}
	log.Info("exam: role granted", "certainty", v.Exam)

	url, err := h.refreshMaterialSource(s)(ctx, m)
	if err != nil && !errors.Is(err, materials.ErrSourceGone) {
		url = m.SourceURL
	}
	embed, components := examCard(m, v, url)
	send := &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed}, Components: components,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}
	if v.Exam == materials.ExamSure {
		send.Flags = discordgo.MessageFlagsSuppressNotifications
	}
	if _, err := s.ChannelMessageSendComplex(config.LogChannelID, send, discordgo.WithContext(ctx)); err != nil {
		log.Error("exam: post report", "error", err)
	}
}

// onExamUndo takes the role back on an admin's click and closes the report.
func (h *Handler) onExamUndo(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := strings.TrimPrefix(i.MessageComponentData().CustomID, examUndoPrefix)
	if !h.InHomeGuild(i.GuildID) || i.Member == nil || !bot.HasAnyRole(i.Member, []string{bot.StaffRoleID, bot.ModRoleID}) {
		h.RespondNow(s, i, "⛔ Μόνο η Ομάδα Διαχείρισης μπορεί να ανακαλέσει τον ρόλο.")
		return
	}
	ctx, cancel := h.CtxTimeout(bot.DBTimeout)
	defer cancel()
	err := h.Retry(ctx, "member.role.remove", func() error {
		return s.GuildMemberRoleRemove(i.GuildID, userID, bot.ExamRoleID, discordgo.WithContext(ctx))
	})
	if err != nil && !dc.IsMissingResource(err) {
		h.Log.Error("exam: revoke role", "user_id", userID, "error", err)
		h.RespondNow(s, i, "Δεν μπόρεσα να αφαιρέσω τον ρόλο. Δοκίμασε ξανά.")
		return
	}
	who := bot.ActorID(i)
	embed := &discordgo.MessageEmbed{Title: "🎓 Ρόλος για θέματα εξετάσεων"}
	if len(i.Message.Embeds) > 0 {
		cp := *i.Message.Embeds[0]
		embed = &cp
	}
	embed.Color = 0xE74C3C
	embed.Description = "↩️ Ο ρόλος αφαιρέθηκε από <@" + who + ">."
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{},
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	}); err != nil {
		h.Log.Warn("exam: update report", "error", err)
	}
	h.Log.Info("exam: role revoked", "user_id", userID, "by", who)
}
