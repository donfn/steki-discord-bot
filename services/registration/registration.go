package registration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/bot"
	"steki-bot/internal/store"
)

// Errors from grantVerification that callers turn into member- or
// moderator-facing messages.
var (
	errIneligibleEmail = errors.New("address does not earn a verified role")
	errRoleNotGranted  = errors.New("verified in the database but the role was not granted")
)

// handleRegistration routes a message sent in a register channel to the step
// the member is currently on.
func (h *Handler) handleRegistration(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer h.LockUser(m.Author.ID)()

	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	reg, err := h.st.RegistrationByChannel(ctx, m.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		// A register channel with no open row: /reg reconcile adopts or
		// removes it.
		h.Log.Warn("message in register channel with no open registration",
			"channel_id", m.ChannelID, "user_id", m.Author.ID)
		return
	}
	if err != nil {
		h.Log.Error("lookup registration failed", "channel_id", m.ChannelID, "error", err)
		return
	}

	// Only the member the channel belongs to drives the flow; moderators can
	// watch without their messages being parsed as answers.
	if reg.UserID != m.Author.ID {
		return
	}

	if err := h.st.Touch(ctx, reg.ID); err != nil {
		h.Log.Warn("touch registration failed", "reg_id", reg.ID, "error", err)
	}

	switch reg.Step {
	case store.StepAwaitingEmail:
		h.postCard(s, m.ChannelID, h.stepSendEmail(ctx, s, m.Author, m.Content, reg))
	case store.StepAwaitingCode:
		h.stepAwaitingLink(ctx, s, m, reg)
	}
}

// stepSendEmail validates the submitted address and mails a fresh code.
func (h *Handler) stepSendEmail(ctx context.Context, s *discordgo.Session,
	author *discordgo.User, input string, reg *store.Registration) card {
	email := cleanEmailInput(input)
	result := func(state cardState) card { return card{userID: reg.UserID, state: state, email: email} }

	if _, ok := classifyEmail(email); !ok {
		h.Audit(reg.UserID, reg.ID, store.EventEmailRejected,
			map[string]any{"input_length": len(input)}, "")
		return result(cardInvalid)
	}

	if reg.CodeSentCount >= maxCodeSends ||
		!h.Gate.Allow("mail-user:"+reg.UserID, maxMailsPerUserHour, time.Hour) {
		h.UrgentModsOnce(s, "toomany:"+reg.UserID, "⚠️ Ο χρήστης <@%s> ζήτησε %d email εγγραφής. Ίσως πρόκειται για κατάχρηση.",
			reg.UserID, reg.CodeSentCount)
		return result(cardTooMany)
	}

	if !h.Gate.Allow("mail-addr:"+email, maxMailsPerAddrHour, time.Hour) ||
		!h.Gate.Allow("mail-all", maxMailsPerHour, time.Hour) {
		return result(cardTooMany)
	}

	if h.emailBanned(ctx, s, reg, email) {
		return result(cardInUse)
	}

	use, err := h.st.CheckEmail(ctx, email)
	if err != nil {
		h.Log.Error("email dedup check failed", "error", err)
		return result(cardFailed)
	}
	if use.BlocksUser(reg.UserID) {
		h.reportUsedEmail(s, reg, email, use)
		return result(cardInUse)
	}

	h.Audit(reg.UserID, reg.ID, store.EventEmailSubmitted, map[string]any{"email": email}, "")

	linkID, err := bot.NewTokenID()
	if err != nil {
		h.Log.Error("generate link id failed", "error", err)
		return result(cardFailed)
	}
	expires := time.Now().Add(linkTTL)
	token, err := h.tokens.IssueLink(reg.UserID, reg.GuildID, linkID, expires)
	if err != nil {
		h.Log.Error("sign link failed", "error", err)
		return result(cardFailed)
	}

	// Send before persisting: if the send fails, the member is still on the
	// email step and can simply try again.
	if err := h.mailer.SendVerificationLink(ctx, h.linkEmail(author, email, token)); err != nil {
		h.Log.Error("send verification email failed", "user_id", reg.UserID, "error", err)
		h.Audit(reg.UserID, reg.ID, store.EventCodeSendFailed,
			map[string]any{"email": email, "error": err.Error()}, "")
		h.UrgentMods(s, "🚨 Απέτυχε η αποστολή email στον <@%s> (`%s`): ```%v```",
			reg.UserID, email, err)
		return result(cardFailed)
	}

	// The link's ID takes the place of the old code: only the newest link
	// sent for a registration works, and only until it expires.
	if err := h.st.BeginCodeStep(ctx, reg.ID, email, linkID, expires); err != nil {
		h.Log.Error("persist link failed", "user_id", reg.UserID, "error", err)
		return result(cardFailed)
	}
	h.Audit(reg.UserID, reg.ID, store.EventCodeSent, map[string]any{"email": email}, "")
	return result(cardSent)
}

// emailBanned reports whether an address was closed by /member ban, recording
// the attempt and telling moderators. A failed lookup counts as not banned: the
// address still goes through the dedup check, and blocking every registration
// on a database hiccup would be worse.
func (h *Handler) emailBanned(ctx context.Context, s *discordgo.Session, reg *store.Registration, email string) bool {
	banned, err := h.st.IsEmailBanned(ctx, email)
	if err != nil {
		h.Log.Error("ban check failed", "error", err)
		return false
	}
	if !banned {
		return false
	}
	h.Audit(reg.UserID, reg.ID, store.EventEmailInUse, map[string]any{"email": email, "banned": true}, "")
	h.AlertModsOnce(s, "banned:"+reg.UserID, "🚫 Ο <@%s> προσπάθησε να εγγραφεί με αποκλεισμένο email: `%s`.", reg.UserID, email)
	return true
}

// reportUsedEmail records an attempt at a taken address and tells moderators
// who holds it.
func (h *Handler) reportUsedEmail(s *discordgo.Session, reg *store.Registration, email string, use store.EmailUse) {
	owner := "άγνωστο"
	if use.UserID != nil {
		owner = "<@" + *use.UserID + ">"
	}
	h.Audit(reg.UserID, reg.ID, store.EventEmailInUse,
		map[string]any{"email": email, "owner": owner, "legacy": use.Legacy}, "")
	h.AlertModsOnce(s, "inuse:"+reg.UserID+email, "ℹ️ Ο <@%s> δοκίμασε το `%s`, που ανήκει ήδη σε %s.",
		reg.UserID, email, owner)
}

// stepAwaitingLink answers a member who types in a register channel opened
// before channels became read-only, while a link is out.
func (h *Handler) stepAwaitingLink(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, reg *store.Registration) {
	if strings.Contains(m.Content, "@") {
		h.postCard(s, m.ChannelID, h.stepSendEmail(ctx, s, m.Author, m.Content, reg))
		return
	}
	if !reg.CodeValid(time.Now()) {
		h.Audit(reg.UserID, reg.ID, store.EventCodeExpired, nil, "")
		if err := h.st.ResetToEmailStep(ctx, reg.ID); err != nil {
			h.Log.Error("reset after expiry failed", "reg_id", reg.ID, "error", err)
		}
		h.postCard(s, m.ChannelID, card{userID: reg.UserID, state: cardExpired})
		return
	}
	h.postCard(s, m.ChannelID, card{userID: reg.UserID, state: cardSent, email: *reg.Email})
}

// completeVerification finishes a registration whose link was accepted and
// removes its channel. Failures are explained in the channel and returned, so
// the link page can say what happened too.
func (h *Handler) completeVerification(ctx context.Context, s *discordgo.Session,
	guildID, channelID string, reg *store.Registration) error {

	if reg.Email == nil {
		h.UrgentMods(s, "⚠️ Ο <@%s> πάτησε τον σύνδεσμο, αλλά δεν υπάρχει email του στη βάση. Ελέγξτε την εγγραφή του.", reg.UserID)
		return errIneligibleEmail
	}
	email := *reg.Email

	// A link sent before the address was banned must not still work.
	if h.emailBanned(ctx, s, reg, email) {
		if channelID != "" {
			h.postCard(s, channelID, card{userID: reg.UserID, state: cardInUse, email: email})
		}
		return errIneligibleEmail
	}

	err := h.grantVerification(ctx, s, guildID, reg, email)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrEmailTaken):
		// Someone else verified the same address after this member submitted
		// it. Send them back to step one rather than leave them on a dead code.
		if rerr := h.st.ResetToEmailStep(ctx, reg.ID); rerr != nil {
			h.Log.Error("reset after email race failed", "reg_id", reg.ID, "error", rerr)
		}
		h.reportUsedEmail(s, reg, email, store.EmailUse{InUse: true})
		if channelID != "" {
			h.postCard(s, channelID, card{userID: reg.UserID, state: cardInUse, email: email})
		}
		return err
	case errors.Is(err, errIneligibleEmail):
		h.UrgentMods(s, "⚠️ Ο <@%s> ολοκλήρωσε την εγγραφή με το `%s`, που δεν είναι αποδεκτό ακαδημαϊκό email. Ελέγξτε την εγγραφή του.",
			reg.UserID, email)
		h.Send(s, channelID, "⚠️ Κάτι πήγε στραβά. Η Ομάδα Διαχείρισης το είδε και θα σε βοηθήσει σύντομα.")
		return err
	case errors.Is(err, errRoleNotGranted):
		// The address is recorded; only the role is missing. Keep the channel
		// so the member is not left with nothing while a moderator steps in.
		h.Log.Error("grant role failed", "user_id", reg.UserID, "error", err)
		h.UrgentMods(s, "🚨 <@&%s> Ο <@%s> επαληθεύτηκε αλλά δεν μπόρεσα να του δώσω ρόλο. "+
			"Δώστε του τον ρόλο χειροκίνητα: ```%v```", bot.StaffRoleID, reg.UserID, err)
		h.Send(s, channelID, "⚠️ Κάτι πήγε στραβά. Η Ομάδα Διαχείρισης το είδε και θα σε βοηθήσει σύντομα.")
		return err
	default:
		// Nothing was written; the link is still valid, so the member can
		// simply accept again.
		h.Log.Error("mark verified failed", "user_id", reg.UserID, "error", err)
		return err
	}

	h.Audit(reg.UserID, reg.ID, store.EventVerified, map[string]any{"email": email}, "")
	h.AlertMods(s, "✅ Ο χρήστης <@%s> ολοκλήρωσε την εγγραφή του με `%s`.", reg.UserID, email)

	h.revokeHelp(ctx, s, reg.UserID)
	if channelID != "" {
		if err := h.DeleteChannel(ctx, s, channelID); err != nil {
			h.Log.Warn("delete register channel failed", "channel_id", channelID, "error", err)
		}
	}
	return nil
}

// grantVerification binds an address to the member and grants the role it
// earns. Shared by the code flow and /reg verify.
//
// The database claim comes first because it is the step that can refuse (the
// address may have been claimed since it was submitted): a member must never
// hold the role for an address the database attributes to someone else. The
// caller must hold the member's lock.
func (h *Handler) grantVerification(ctx context.Context, s *discordgo.Session,
	guildID string, reg *store.Registration, email string) error {

	roleID, ok := classifyEmail(email)
	if !ok {
		return errIneligibleEmail
	}
	if err := h.st.MarkVerified(ctx, reg.ID, reg.UserID, email); err != nil {
		return err
	}
	if err := h.GrantRole(ctx, s, guildID, reg.UserID, roleID); err != nil {
		return fmt.Errorf("%w: %w", errRoleNotGranted, err)
	}
	return nil
}

// restartRegistration wipes the member's progress and replays the welcome.
func (h *Handler) restartRegistration(s *discordgo.Session, channelID, userID, guildID string) error {
	defer h.LockUser(userID)()

	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	reg, err := h.st.RegistrationByChannel(ctx, channelID)
	if errors.Is(err, store.ErrNotFound) {
		// The channel outlived its row; bind it to a fresh registration.
		reg, err = h.adoptChannel(ctx, guildID, userID, channelID)
	}
	if err != nil {
		return fmt.Errorf("find registration: %w", err)
	}

	if err := h.st.ResetToEmailStep(ctx, reg.ID); err != nil {
		return fmt.Errorf("reset registration: %w", err)
	}
	h.Audit(userID, reg.ID, store.EventRestarted, nil, "")

	// Channels opened before they became read-only switch over here.
	ow := memberOverwrite(userID)
	if err := s.ChannelPermissionSet(channelID, userID, ow.Type, ow.Allow, ow.Deny,
		discordgo.WithContext(ctx)); err != nil {
		h.Log.Warn("restart: make channel read-only failed", "channel_id", channelID, "error", err)
	}

	h.revokeHelp(ctx, s, userID)
	h.purgeChannel(ctx, s, channelID)
	h.postFlowIntro(s, channelID, userID)
	return nil
}

// adoptChannel binds an existing register channel to the member's open
// registration, creating the member and the registration if need be.
func (h *Handler) adoptChannel(ctx context.Context, guildID, userID, channelID string) (*store.Registration, error) {
	if err := h.st.EnsureMember(ctx, userID, ""); err != nil {
		return nil, err
	}
	reg, _, err := h.st.EnsureOpenRegistration(ctx, userID, guildID)
	if err != nil {
		return nil, err
	}
	if err := h.st.AttachChannel(ctx, reg.ID, channelID, ""); err != nil {
		return nil, err
	}
	return reg, nil
}

// purgeChannel clears a register channel's history before replaying the flow.
// Discord refuses to bulk-delete messages older than two weeks, so those are
// left in place rather than failing the restart.
func (h *Handler) purgeChannel(ctx context.Context, s *discordgo.Session, channelID string) {
	msgs, err := s.ChannelMessages(channelID, 100, "", "", "", discordgo.WithContext(ctx))
	if err != nil {
		h.Log.Warn("purge: list messages failed", "channel_id", channelID, "error", err)
		return
	}
	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	var ids []string
	for _, msg := range msgs {
		if msg.Timestamp.After(cutoff) {
			ids = append(ids, msg.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := s.ChannelMessagesBulkDelete(channelID, ids, discordgo.WithContext(ctx)); err != nil {
		h.Log.Warn("purge channel failed", "channel_id", channelID, "error", err)
	}
}
