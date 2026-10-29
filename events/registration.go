package events

import (
	"context"
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
		h.stepSendEmail(ctx, s, m.ChannelID, m.Author, m.Content, reg)
	case store.StepAwaitingCode:
		h.stepAwaitingLink(ctx, s, m, reg)
	}
}

// stepSendEmail validates the submitted address and mails a fresh code.
func (h *Handler) stepSendEmail(ctx context.Context, s *discordgo.Session, channelID string,
	author *discordgo.User, input string, reg *store.Registration) {
	email := cleanEmailInput(input)

	if _, ok := classifyEmail(email); !ok {
		h.audit(reg.UserID, reg.ID, store.EventEmailRejected,
			map[string]any{"input_length": len(input)}, "")
		h.send(s, channelID,
			":see_no_evil: Αυτό που μου έστειλες δεν μοιάζει με φοιτητικό email από το H.M.M.Y. ΕΛ.ΜΕ.ΠΑ.")
		h.sendWithEmailButton(s, channelID,
			"Ένα ακαδημαϊκό, φοιτητικό email έχει την μορφή thXXXXX@edu.hmu.gr. Πάτα το κουμπί για να το ξαναγράψεις.",
			labelRetryEmail)
		return
	}

	if reg.CodeSentCount >= maxCodeSends {
		h.alertMods(s, "⚠️ Ο χρήστης <@%s> ζήτησε %d κωδικούς. Πιθανή κατάχρηση.",
			reg.UserID, reg.CodeSentCount)
		h.send(s, channelID, fmt.Sprintf(
			"Έχεις ζητήσει πολλούς κωδικούς. Στείλε μήνυμα στο <#%s> για βοήθεια.",
			config.SupportChannelID))
		return
	}

	use, err := h.st.CheckEmail(ctx, email)
	if err != nil {
		h.log.Error("email dedup check failed", "error", err)
		h.send(s, channelID, msgGenericError)
		return
	}
	if use.BlocksUser(reg.UserID) {
		h.rejectUsedEmail(s, channelID, reg, email, use)
		return
	}

	h.audit(reg.UserID, reg.ID, store.EventEmailSubmitted, map[string]any{"email": email}, "")
	h.send(s, channelID, "Δώσε μου μισό λεπτάκι...")

	linkID, err := newTokenID()
	if err != nil {
		h.log.Error("generate link id failed", "error", err)
		h.send(s, channelID, msgGenericError)
		return
	}
	expires := time.Now().Add(linkTTL)
	token, err := h.tokens.IssueLink(reg.UserID, reg.GuildID, linkID, expires)
	if err != nil {
		h.log.Error("sign link failed", "error", err)
		h.send(s, channelID, msgGenericError)
		return
	}

	// Send before persisting: if the send fails, the member is still on the
	// email step and can simply try again.
	if err := h.mailer.SendVerificationLink(ctx, h.linkEmail(author, email, token)); err != nil {
		h.log.Error("send verification email failed", "user_id", reg.UserID, "error", err)
		h.audit(reg.UserID, reg.ID, store.EventCodeSendFailed,
			map[string]any{"email": email, "error": err.Error()}, "")
		h.send(s, channelID, msgGenericError)
		h.alertMods(s, "🚨 Απέτυχε η αποστολή email στον <@%s> (`%s`): ```%v```",
			reg.UserID, email, err)
		return
	}

	// The link's ID takes the place of the old code: only the newest link
	// sent for a registration works, and only until it expires.
	if err := h.st.BeginCodeStep(ctx, reg.ID, email, linkID, expires); err != nil {
		h.log.Error("persist link failed", "user_id", reg.UserID, "error", err)
		h.send(s, channelID, msgGenericError)
		return
	}
	h.audit(reg.UserID, reg.ID, store.EventCodeSent, map[string]any{"email": email}, "")

	if hmuEmailRegex.MatchString(email) {
		h.send(s, channelID,
			"Τέλεια! Τσέκαρε τα εισερχόμενά σου (https://webmail.edu.hmu.gr) "+
				"για ένα μήνυμα με θέμα *\"Εγγραφή στο Steki\"*")
	} else {
		h.send(s, channelID,
			"Το email που έστειλες αντιστοιχεί σε εξωτερικό τμήμα. "+
				"Γι' αυτό, θα γραφτείς ως Verified Outsider. Τσέκαρε το ακαδημαϊκό σου email.")
	}
	h.sendWithEmailButton(s, channelID, fmt.Sprintf(
		"Πάτα το κουμπί **Σύνδεση λογαριασμού** στο email και μετά **Αποδοχή** στη σελίδα που θα ανοίξει. "+
			"Θα μπεις αυτόματα. Ο σύνδεσμος ισχύει για %d λεπτά.\n\n"+
			"Έκανες λάθος στη διεύθυνση ή δεν ήρθε το email; Πάτα το κουμπί για να στείλεις άλλη.",
		int(linkTTL.Minutes())), labelChangeEmail)
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

// stepAwaitingLink answers a member who writes in their channel while a link
// is out. A new address replaces the pending one; anything else gets a
// reminder of where the link is.
func (h *Handler) stepAwaitingLink(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, reg *store.Registration) {
	if strings.Contains(m.Content, "@") {
		h.stepSendEmail(ctx, s, m.ChannelID, m.Author, m.Content, reg)
		return
	}
	if !reg.CodeValid(time.Now()) {
		h.audit(reg.UserID, reg.ID, store.EventCodeExpired, nil, "")
		if err := h.st.ResetToEmailStep(ctx, reg.ID); err != nil {
			h.log.Error("reset after expiry failed", "reg_id", reg.ID, "error", err)
		}
		h.sendWithEmailButton(s, m.ChannelID,
			"⌛ Ο σύνδεσμος που σου έστειλα έληξε. Πάτα το κουμπί και γράψε ξανά το ακαδημαϊκό σου email για να σου στείλω καινούριο.",
			labelRetryEmail)
		return
	}
	h.sendWithEmailButton(s, m.ChannelID, fmt.Sprintf(
		"Σου έστειλα ήδη έναν σύνδεσμο στο **%s**. Πάτα **Σύνδεση λογαριασμού** στο email για να μπεις. "+
			"Αν έκανες λάθος στη διεύθυνση, πάτα το κουμπί για να στείλεις τη σωστή.", *reg.Email), labelChangeEmail)
}

// completeVerification finishes a registration whose link was accepted and
// removes its channel. Failures are explained in the channel and returned, so
// the link page can say what happened too.
func (h *Handler) completeVerification(ctx context.Context, s *discordgo.Session,
	guildID, channelID string, reg *store.Registration) error {

	if reg.Email == nil {
		h.alertMods(s, "⚠️ Ο <@%s> αποδέχτηκε τον σύνδεσμο αλλά δεν υπάρχει email στη βάση. Χρειάζεται έλεγχος.", reg.UserID)
		return errIneligibleEmail
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
		return err
	case errors.Is(err, errIneligibleEmail):
		h.alertMods(s, "⚠️ Κατά την εγγραφή του <@%s> το mail `%s` δεν πληροί τις προϋποθέσεις. Please check.",
			reg.UserID, email)
		h.send(s, channelID, "Κάτι πήγε στραβά. Δώσε λίγη ώρα για την Ομάδα Διαχείρισης να το ελέγξει...")
		return err
	case errors.Is(err, errRoleNotGranted):
		// The address is recorded; only the role is missing. Keep the channel
		// so the member is not left with nothing while a moderator steps in.
		h.log.Error("grant role failed", "user_id", reg.UserID, "error", err)
		h.alertMods(s, "🚨 <@&%s> Ο <@%s> επαληθεύτηκε αλλά δεν μπόρεσα να του δώσω ρόλο. "+
			"Χρειάζεται χειροκίνητα: ```%v```", staffRoleID, reg.UserID, err)
		h.send(s, channelID, "Κάτι πήγε στραβά. Δώσε λίγη ώρα για την Ομάδα Διαχείρισης να το ελέγξει...")
		return err
	default:
		// Nothing was written; the link is still valid, so the member can
		// simply accept again.
		h.log.Error("mark verified failed", "user_id", reg.UserID, "error", err)
		return err
	}

	h.audit(reg.UserID, reg.ID, store.EventVerified, map[string]any{"email": email}, "")
	h.alertMods(s, "✅ Ο χρήστης <@%s> ολοκλήρωσε με επιτυχία την εγγραφή του με `%s`.", reg.UserID, email)

	if channelID != "" {
		if err := h.deleteChannel(ctx, s, channelID); err != nil {
			h.log.Warn("delete register channel failed", "channel_id", channelID, "error", err)
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

	// Channels opened before they became read-only switch over here.
	ow := memberOverwrite(userID)
	if err := s.ChannelPermissionSet(channelID, userID, ow.Type, ow.Allow, ow.Deny,
		discordgo.WithContext(ctx)); err != nil {
		h.log.Warn("restart: make channel read-only failed", "channel_id", channelID, "error", err)
	}

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
