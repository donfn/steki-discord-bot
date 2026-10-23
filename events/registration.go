package events

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
	"steki-bot/store"
)

// Errors from grantVerification that callers turn into member- or
// moderator-facing messages.
var (
	errIneligibleEmail = errors.New("address does not earn a verified role")
	errRoleNotGranted  = errors.New("verified in the database but the role was not granted")
)

const msgGenericError = "Υπήρξε ένα σφάλμα. Παρακαλώ προσπάθησε αργότερα..."

// handleRegistration routes a message sent in a register channel to the step
// the member is currently on.
func (h *Handler) handleRegistration(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer h.lockUser(m.Author.ID)()

	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	reg, err := h.st.RegistrationByChannel(ctx, m.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		// A register channel with no open row: the reconciler adopts or
		// removes it on its next pass.
		h.log.Warn("message in register channel with no open registration",
			"channel_id", m.ChannelID, "user_id", m.Author.ID)
		return
	}
	if err != nil {
		h.log.Error("lookup registration failed", "channel_id", m.ChannelID, "error", err)
		return
	}

	// Only the member the channel belongs to drives the flow; moderators can
	// watch without their messages being parsed as answers.
	if reg.UserID != m.Author.ID {
		return
	}

	if err := h.st.Touch(ctx, reg.ID); err != nil {
		h.log.Warn("touch registration failed", "reg_id", reg.ID, "error", err)
	}

	switch reg.Step {
	case store.StepAwaitingEmail:
		h.stepSendEmail(ctx, s, m, reg)
	case store.StepAwaitingCode:
		h.stepVerifyCode(ctx, s, m, reg)
	}
}

// stepSendEmail validates the submitted address and mails a fresh code.
func (h *Handler) stepSendEmail(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, reg *store.Registration) {
	email := store.NormalizeEmail(m.Content)

	if _, ok := classifyEmail(email); !ok {
		h.audit(reg.UserID, reg.ID, store.EventEmailRejected,
			map[string]any{"input_length": len(m.Content)}, "")
		h.send(s, m.ChannelID,
			":see_no_evil: Αυτό που μου έστειλες δεν μοιάζει με φοιτητικό email από το H.M.M.Y. ΕΛ.ΜΕ.ΠΑ.")
		h.send(s, m.ChannelID,
			"Ένα ακαδημαϊκό, φοιτητικό email έχει την μορφή thXXXXX@edu.hmu.gr.")
		return
	}

	if reg.CodeSentCount >= maxCodeSends {
		h.alertMods(s, "⚠️ Ο χρήστης <@%s> ζήτησε %d κωδικούς. Πιθανή κατάχρηση.",
			reg.UserID, reg.CodeSentCount)
		h.send(s, m.ChannelID, fmt.Sprintf(
			"Έχεις ζητήσει πολλούς κωδικούς. Στείλε μήνυμα στο <#%s> για βοήθεια.",
			config.SupportChannelID))
		return
	}

	use, err := h.st.CheckEmail(ctx, email)
	if err != nil {
		h.log.Error("email dedup check failed", "error", err)
		h.send(s, m.ChannelID, msgGenericError)
		return
	}
	if use.BlocksUser(reg.UserID) {
		h.rejectUsedEmail(s, m.ChannelID, reg, email, use)
		return
	}

	h.audit(reg.UserID, reg.ID, store.EventEmailSubmitted, map[string]any{"email": email}, "")
	h.send(s, m.ChannelID, "Δώσε μου μισό λεπτάκι...")

	code, err := generateCode()
	if err != nil {
		h.log.Error("generate code failed", "error", err)
		h.send(s, m.ChannelID, msgGenericError)
		return
	}

	// Send before persisting: if the send fails, the member is still on the
	// email step and can simply try again.
	if err := h.mailer.SendVerificationCode(ctx, email, code); err != nil {
		h.log.Error("send verification email failed", "user_id", reg.UserID, "error", err)
		h.audit(reg.UserID, reg.ID, store.EventCodeSendFailed,
			map[string]any{"email": email, "error": err.Error()}, "")
		h.send(s, m.ChannelID, msgGenericError)
		h.alertMods(s, "🚨 Απέτυχε η αποστολή email στον <@%s> (`%s`): ```%v```",
			reg.UserID, email, err)
		return
	}

	if err := h.st.BeginCodeStep(ctx, reg.ID, email, code, time.Now().Add(codeTTL)); err != nil {
		h.log.Error("persist code failed", "user_id", reg.UserID, "error", err)
		h.send(s, m.ChannelID, msgGenericError)
		return
	}
	h.audit(reg.UserID, reg.ID, store.EventCodeSent, map[string]any{"email": email}, "")

	if hmuEmailRegex.MatchString(email) {
		h.send(s, m.ChannelID,
			"Τέλεια! Τσέκαρε τα εισερχόμενά σου (https://webmail.edu.hmu.gr) "+
				"για ένα μήνυμα με θέμα *\"Εγγραφή στο Steki\"*")
	} else {
		h.send(s, m.ChannelID,
			"Το email που έστειλες αντιστοιχεί σε εξωτερικό τμήμα. "+
				"Γι' αυτό, θα γραφτείς ως Verified Outsider. Τσέκαρε το ακαδημαϊκό σου email.")
	}
	h.send(s, m.ChannelID, "Όταν το λάβεις, στείλε μου τον κωδικό εγγραφής εδώ. Ισχύει για 15 λεπτά.")
}

// rejectUsedEmail tells the member an address is taken and tells moderators who
// holds it.
func (h *Handler) rejectUsedEmail(s *discordgo.Session, channelID string, reg *store.Registration,
	email string, use store.EmailUse) {

	owner := "άγνωστο"
	if use.UserID != nil {
		owner = "<@" + *use.UserID + ">"
	}
	h.audit(reg.UserID, reg.ID, store.EventEmailInUse,
		map[string]any{"email": email, "owner": owner, "legacy": use.Legacy}, "")
	h.alertMods(s, "ℹ️ Ο <@%s> δοκίμασε το `%s`, που ανήκει ήδη σε %s.",
		reg.UserID, email, owner)
	h.send(s, channelID, fmt.Sprintf(
		"Αυτό το email έχει ήδη χρησιμοποιηθεί για εγγραφή στο Steki. "+
			"Αν θεωρείς ότι έχει γίνει κάποιο λάθος, στείλε ένα μήνυμα στο <#%s>.",
		config.SupportChannelID))
}

// stepVerifyCode checks the submitted code and, on success, grants the role.
func (h *Handler) stepVerifyCode(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, reg *store.Registration) {
	input := strings.TrimSpace(m.Content)

	if !reg.CodeValid(time.Now()) {
		h.audit(reg.UserID, reg.ID, store.EventCodeExpired, nil, "")
		if err := h.st.ResetToEmailStep(ctx, reg.ID); err != nil {
			h.log.Error("reset after expiry failed", "reg_id", reg.ID, "error", err)
		}
		h.send(s, m.ChannelID,
			"⌛ Ο κωδικός σου έληξε. Στείλε μου ξανά το ακαδημαϊκό σου email για να σου στείλω καινούριο.")
		return
	}

	if subtle.ConstantTimeCompare([]byte(input), []byte(*reg.Code)) == 1 {
		h.completeVerification(ctx, s, m.GuildID, m.ChannelID, reg)
		return
	}

	attempts, err := h.st.IncrementFailedAttempts(ctx, reg.ID)
	if err != nil {
		h.log.Error("increment attempts failed", "reg_id", reg.ID, "error", err)
		h.send(s, m.ChannelID, msgGenericError)
		return
	}
	h.audit(reg.UserID, reg.ID, store.EventCodeWrong, map[string]any{"attempts": attempts}, "")

	if attempts >= maxFailedAttempts {
		if err := h.st.ResetToEmailStep(ctx, reg.ID); err != nil {
			h.log.Error("reset after failures failed", "reg_id", reg.ID, "error", err)
		}
		h.alertMods(s, "⚠️ Ο χρήστης <@%s> έβαλε %d φορές λάθος κωδικό. Η διαδικασία επανεκκινήθηκε.",
			reg.UserID, attempts)
		h.send(s, m.ChannelID,
			"Για λόγους ασφαλείας ο κωδικός εγγραφής σου έχει καταστραφεί. "+
				"Στείλε μου ξανά το ακαδημαϊκό σου email για να ξεκινήσουμε από την αρχή.")
		return
	}

	h.send(s, m.ChannelID, fmt.Sprintf(
		"Χμμμ... Αυτή δεν ήταν η απάντηση που περίμενα. "+
			"Χρειάζομαι *μόνο* τον 6-ψήφιο κωδικό εγγραφής που αναγράφει το mail :woozy_face:. "+
			"(προσπάθεια %d από %d)", attempts, maxFailedAttempts))
}

// completeVerification finishes a registration whose code was correct and
// removes its channel.
func (h *Handler) completeVerification(ctx context.Context, s *discordgo.Session,
	guildID, channelID string, reg *store.Registration) {

	if reg.Email == nil {
		h.alertMods(s, "⚠️ Ο <@%s> πέρασε τον κωδικό αλλά δεν υπάρχει email στη βάση. Χρειάζεται έλεγχος.", reg.UserID)
		return
	}
	email := *reg.Email

	err := h.grantVerification(ctx, s, guildID, reg, email)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrEmailTaken):
		// Someone else verified the same address after this member submitted
		// it. Send them back to step one rather than leave them on a dead code.
		if rerr := h.st.ResetToEmailStep(ctx, reg.ID); rerr != nil {
			h.log.Error("reset after email race failed", "reg_id", reg.ID, "error", rerr)
		}
		h.rejectUsedEmail(s, channelID, reg, email, store.EmailUse{InUse: true})
		return
	case errors.Is(err, errIneligibleEmail):
		h.alertMods(s, "⚠️ Κατά την εγγραφή του <@%s> το mail `%s` δεν πληροί τις προϋποθέσεις. Please check.",
			reg.UserID, email)
		h.send(s, channelID, "Κάτι πήγε στραβά. Δώσε λίγη ώρα για την Ομάδα Διαχείρισης να το ελέγξει...")
		return
	case errors.Is(err, errRoleNotGranted):
		// The address is recorded; only the role is missing. Keep the channel
		// so the member is not left with nothing while a moderator steps in.
		h.log.Error("grant role failed", "user_id", reg.UserID, "error", err)
		h.alertMods(s, "🚨 <@&%s> Ο <@%s> επαληθεύτηκε αλλά δεν μπόρεσα να του δώσω ρόλο. "+
			"Χρειάζεται χειροκίνητα: ```%v```", staffRoleID, reg.UserID, err)
		h.send(s, channelID, "Κάτι πήγε στραβά. Δώσε λίγη ώρα για την Ομάδα Διαχείρισης να το ελέγξει...")
		return
	default:
		// Nothing was written; the code is still valid, so the member can
		// simply send it again.
		h.log.Error("mark verified failed", "user_id", reg.UserID, "error", err)
		h.send(s, channelID, msgGenericError)
		return
	}

	h.audit(reg.UserID, reg.ID, store.EventVerified, map[string]any{"email": email}, "")
	h.alertMods(s, "✅ Ο χρήστης <@%s> ολοκλήρωσε με επιτυχία την εγγραφή του με `%s`.", reg.UserID, email)

	if err := h.deleteChannel(ctx, s, channelID); err != nil {
		h.log.Warn("delete register channel failed", "channel_id", channelID, "error", err)
	}
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
	if err := h.grantRole(ctx, s, guildID, reg.UserID, roleID); err != nil {
		return fmt.Errorf("%w: %w", errRoleNotGranted, err)
	}
	return nil
}

// restartRegistration wipes the member's progress and replays the welcome.
func (h *Handler) restartRegistration(s *discordgo.Session, channelID, userID, guildID string) error {
	defer h.lockUser(userID)()

	ctx, cancel := h.ctxTimeout(flowTimeout)
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
	h.audit(userID, reg.ID, store.EventRestarted, nil, "")

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
		h.log.Warn("purge: list messages failed", "channel_id", channelID, "error", err)
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
		h.log.Warn("purge channel failed", "channel_id", channelID, "error", err)
	}
}
