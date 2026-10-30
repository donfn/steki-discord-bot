package events

import (
	"errors"
	"strings"

	"github.com/bwmarrin/discordgo"

	"steki-bot/store"
)

// Register channels are read-only for the member: they give their address
// through a Discord form opened by the card's button, and restart with
// another.
// Free text can no longer be mistaken for an answer, and nobody can post
// links or spam into their own channel.
const (
	emailButtonID   = "reg:email"
	restartButtonID = "reg:restart"
	emailFormID     = "reg:email_form"
	emailInputID    = "email"
)

// invisibles are characters that ride along when an address is copied from a
// web page or a PDF; the full-width at sign comes from Greek and Asian
// keyboards.
var invisibles = strings.NewReplacer(
	"\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "", "\u00ad", "",
	"\uff20", "@",
)

// cleanEmailInput turns what a member typed into the address they meant:
// lowercase, with no spaces or invisible characters anywhere, and without a
// mailto: prefix or surrounding quotes, brackets and punctuation.
func cleanEmailInput(input string) string {
	s := invisibles.Replace(input)
	s = strings.Join(strings.Fields(s), "")
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "mailto:")
	s = strings.Trim(s, `<>"'`+"`"+`.,;:()[]`)
	return store.NormalizeEmail(s)
}

// interactionUser is whoever clicked or submitted.
func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

// ownRegistration returns the open registration of the channel the
// interaction came from, when it belongs to the member who sent it. Anyone
// else gets an ephemeral explanation and nil.
func (h *Handler) ownRegistration(s *discordgo.Session, i *discordgo.InteractionCreate) *store.Registration {
	user := interactionUser(i)
	if user == nil || !h.inHomeGuild(i.GuildID) {
		return nil
	}
	ctx, cancel := h.ctx()
	defer cancel()

	reg, err := h.st.RegistrationByChannel(ctx, i.ChannelID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.respondNow(s, i, "Αυτή η εγγραφή δεν είναι πια ενεργή.")
		return nil
	case err != nil:
		h.log.Error("lookup registration failed", "channel_id", i.ChannelID, "error", err)
		h.respondNow(s, i, msgGenericError)
		return nil
	case reg.UserID != user.ID:
		h.respondNow(s, i, "Αυτό το κουμπί είναι μόνο για το μέλος που γράφεται σε αυτό το κανάλι.")
		return nil
	}
	return reg
}

// onComponent handles the register channel buttons.
func (h *Handler) onComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if id := i.MessageComponentData().CustomID; strings.HasPrefix(id, reviewApprovePrefix) || strings.HasPrefix(id, reviewRejectPrefix) {
		h.onReviewButton(s, i)
		return
	}
	if id := i.MessageComponentData().CustomID; strings.HasPrefix(id, reportRemovePrefix) || strings.HasPrefix(id, reportRestorePrefix) {
		h.onReportButton(s, i)
		return
	}
	switch i.MessageComponentData().CustomID {
	case uploadButtonID:
		h.openUploadForm(s, i)
	case activityButtonID:
		h.launchActivity(s, i)
	case emailButtonID:
		if h.ownRegistration(s, i) == nil {
			return
		}
		h.openEmailForm(s, i)
	case helpButtonID:
		if reg := h.ownRegistration(s, i); reg != nil {
			h.onHelpButton(s, i, reg)
		}
	case restartButtonID:
		reg := h.ownRegistration(s, i)
		if reg == nil {
			return
		}
		// The restart purges the channel, this message included, so the
		// click is acknowledged without a reply.
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredMessageUpdate,
		}); err != nil {
			h.log.Warn("acknowledge restart failed", "error", err)
		}
		if err := h.restartRegistration(s, i.ChannelID, reg.UserID, i.GuildID); err != nil {
			h.log.Error("restart registration failed", "user_id", reg.UserID, "error", err)
		}
	}
}

// openEmailForm shows the native Discord form for the address.
func (h *Handler) openEmailForm(s *discordgo.Session, i *discordgo.InteractionCreate) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: emailFormID,
			Title:    "Εγγραφή στο Steki",
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{
					CustomID:    emailInputID,
					Label:       "Το ακαδημαϊκό σου email",
					Style:       discordgo.TextInputShort,
					Placeholder: "thXXXXX@edu.hmu.gr",
					Required:    true,
					MinLength:   6,
					MaxLength:   100,
				},
			}}},
		},
	})
	if err != nil {
		h.log.Error("open email form failed", "error", err)
	}
}

// onEmailSubmit shows the cleaned-up address on the card, sends the email,
// and redraws the card with the outcome.
func (h *Handler) onEmailSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	if data.CustomID != emailFormID {
		return
	}
	reg := h.ownRegistration(s, i)
	if reg == nil {
		return
	}
	raw := modalValue(data.Components, emailInputID)
	user := interactionUser(i)

	// Redraw the card at once with the address echoed back, then again once
	// the email has gone out. A form opened from a message may update it.
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Components: card{userID: user.ID, state: cardSending, email: cleanEmailInput(raw)}.components(),
			Flags:      discordgo.MessageFlagsIsComponentsV2,
		},
	})
	if err != nil {
		h.log.Warn("update registration card failed", "error", err)
	}

	defer h.lockUser(user.ID)()
	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	// Re-read under the lock: a restart or an accepted link may have closed
	// the registration while the form was open.
	reg, err = h.st.RegistrationByChannel(ctx, i.ChannelID)
	if err != nil || reg.UserID != user.ID {
		return
	}
	if err := h.st.Touch(ctx, reg.ID); err != nil {
		h.log.Warn("touch registration failed", "reg_id", reg.ID, "error", err)
	}
	result := h.stepSendEmail(ctx, s, user, raw, reg)
	if i.Message != nil {
		h.editCard(s, i.ChannelID, i.Message.ID, result)
	} else {
		h.postCard(s, i.ChannelID, result)
	}
}

// modalValue finds a text input's value among a submitted form's rows.
func modalValue(rows []discordgo.MessageComponent, id string) string {
	for _, row := range rows {
		r, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range r.Components {
			if in, ok := c.(*discordgo.TextInput); ok && in.CustomID == id {
				return in.Value
			}
		}
	}
	return ""
}
