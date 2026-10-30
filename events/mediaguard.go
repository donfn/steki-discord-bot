package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/moderation"
	"steki-bot/store"
	"steki-bot/verify"
)

// The media guard. Hijacked accounts are used to spray scam images and links
// across every channel within minutes, so a member who posts an attachment or
// a link twice within an hour is timed out for a day and sent a captcha link.
// Solving it lifts the timeout at once and exempts them for a day.
const (
	guardWindow      = time.Hour
	guardTimeout     = 24 * time.Hour
	guardExemption   = 24 * time.Hour
	guardButtonLabel = "Αφαίρεση περιορισμών"
	guardColor       = 0xFFC900
)

// flaggedPost reports whether a message carries an attachment or a link.
func flaggedPost(m *discordgo.Message) bool {
	return len(moderation.Attachments(m)) > 0 || moderation.ContainsURL(moderation.Text(m))
}

// screenMedia counts a flagged post and restricts the member on the second
// one within the window. It reports whether the member was restricted.
func (h *Handler) screenMedia(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if !flaggedPost(m.Message) || h.isModerator(s, m.Author.ID, m.ChannelID) {
		return false
	}

	tokenID, err := newTokenID()
	if err != nil {
		h.log.Error("media guard: token id", "error", err)
		return false
	}

	ctx, cancel := h.ctx()
	tripped, err := h.st.RecordFlaggedPost(ctx, m.Author.ID, m.GuildID, tokenID, time.Now(), guardWindow)
	cancel()
	if err != nil {
		h.log.Error("media guard: record post", "user_id", m.Author.ID, "error", err)
		return false
	}
	if !tripped {
		return false
	}
	_, _ = h.restrict(s, m.Author.ID, m.GuildID, m.ChannelID, verify.ReasonMedia, tokenID, "")
	return true
}

// isModerator exempts members who can moderate the channel anyway; Discord
// would refuse to time out an administrator in any case.
func (h *Handler) isModerator(s *discordgo.Session, userID, channelID string) bool {
	perms, err := s.State.UserChannelPermissions(userID, channelID)
	if err != nil {
		perms, err = s.UserChannelPermissions(userID, channelID)
		if err != nil {
			return false
		}
	}
	return perms&(discordgo.PermissionAdministrator|discordgo.PermissionManageMessages) != 0
}

// restrictionSubject opens the mod log line for a restriction: who, and why.
func restrictionSubject(userID, channelID, reason, actorID string) string {
	switch reason {
	case verify.ReasonManual:
		return fmt.Sprintf("Ο <@%s> έβαλε τον χρήστη <@%s> σε έλεγχο captcha", actorID, userID)
	case verify.ReasonDuplicate:
		return fmt.Sprintf("Ο χρήστης <@%s> έστειλε το ίδιο μήνυμα σε πολλά κανάλια μέσα σε ένα λεπτό "+
			"(τελευταίο στο <#%s>, τα μηνύματα διαγράφηκαν)", userID, channelID)
	default:
		return fmt.Sprintf("Ο χρήστης <@%s> έστειλε δύο μηνύματα με συνημμένα ή συνδέσμους μέσα σε μία ώρα "+
			"(τελευταίο στο <#%s>)", userID, channelID)
	}
}

// restrictResult reports how a restriction went, for callers that answer a
// moderator.
type restrictResult struct {
	Link   string
	DMSent bool
}

// restrict times the member out and DMs them the verification button. The
// caller has already recorded tokenID as the member's outstanding link.
// actorID is the moderator for a manual restriction, otherwise empty.
func (h *Handler) restrict(s *discordgo.Session, userID, guildID, channelID, reason, tokenID, actorID string) (restrictResult, error) {
	until := time.Now().Add(guardTimeout)
	subject := restrictionSubject(userID, channelID, reason, actorID)

	ctx, cancel := h.ctx()
	defer cancel()

	err := h.retry(ctx, "member.timeout", func() error {
		return s.GuildMemberTimeout(guildID, userID, &until, discordgo.WithContext(ctx))
	})
	if err != nil {
		h.log.Warn("media guard: timeout failed", "user_id", userID, "error", err)
		h.urgentMods(s, "⚠️ %s, αλλά το timeout απέτυχε: `%v`", subject, err)
		return restrictResult{}, fmt.Errorf("timeout: %w", err)
	}

	token, err := h.tokens.Issue(userID, guildID, reason, tokenID, until)
	if err != nil {
		h.log.Error("media guard: issue token", "user_id", userID, "error", err)
		return restrictResult{}, fmt.Errorf("issue link: %w", err)
	}
	res := restrictResult{Link: h.cfg.PublicURL + "/verify?t=" + url.QueryEscape(token)}

	h.audit(userID, "", store.EventMediaRestricted, map[string]any{
		"channel_id": channelID, "reason": reason, "until": until,
	}, actorID)
	h.log.Info("media guard: restricted", "user_id", userID, "channel_id", channelID, "reason", reason)

	dm, err := h.sendGuardDM(s, userID, reason, res.Link)
	if err != nil {
		h.log.Info("media guard: DM failed", "user_id", userID, "error", err)
		h.urgentMods(s, "🛡️ %s. Timeout 24 ωρών, αλλά **δεν δέχεται DM**, οπότε δεν πήρε τον σύνδεσμο "+
			"επιβεβαίωσης. Μπορείτε να του τον στείλετε ή να αφαιρέσετε το timeout χειροκίνητα:\n<%s>",
			subject, res.Link)
		return res, nil
	}
	res.DMSent = true
	if err := h.st.SetGuardDM(ctx, userID, tokenID, dm); err != nil {
		h.log.Warn("media guard: save DM", "user_id", userID, "error", err)
	}
	h.alertMods(s, "🛡️ %s. Timeout 24 ωρών, μέχρι να λύσει το captcha που του στάλθηκε σε DM.", subject)
	return res, nil
}

// guardMessage is the DM explaining the timeout. With disabled set it is the
// same message after the member has verified, its button greyed out.
func guardMessage(reason, link string, disabled bool) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	what := "Ο λογαριασμός σου έστειλε **δύο μηνύματα με συνημμένα ή συνδέσμους μέσα σε μία ώρα**, " +
		"οπότε μπήκε αυτόματα σε timeout για 24 ώρες."
	switch reason {
	case verify.ReasonDuplicate:
		what = "Ο λογαριασμός σου έστειλε **το ίδιο μήνυμα σε πολλά κανάλια μέσα σε ένα λεπτό**, " +
			"οπότε τα μηνύματα διαγράφηκαν και μπήκε αυτόματα σε timeout για 24 ώρες."
	case verify.ReasonManual:
		what = "**Η Ομάδα Διαχείρισης έβαλε τον λογαριασμό σου σε timeout για 24 ώρες**, επειδή " +
			"η δραστηριότητά του μοιάζει με λογαριασμό που έχει παραβιαστεί."
	}
	embed := &discordgo.MessageEmbed{
		Title: "Προσωρινός περιορισμός στο στέκι",
		Color: guardColor,
		Description: what + "\n\n" +
			"**Γιατί;** Όταν κάποιος παραβιάσει έναν λογαριασμό Discord, συνήθως στέλνει αμέσως μηνύματα, " +
			"εικόνες και συνδέσμους απάτης σε όλα τα κανάλια. Αυτός ο έλεγχος σταματά τέτοιες επιθέσεις πριν φτάσουν στα μέλη.\n\n" +
			"Αν ήσουν εσύ, πάτα το κουμπί και λύσε έναν σύντομο έλεγχο για να αφαιρεθεί ο περιορισμός αμέσως.\n\n" +
			"**Δεν έστειλες εσύ αυτά τα μηνύματα;** Άλλαξε άμεσα τον κωδικό σου και ενεργοποίησε το 2FA.",
	}
	if disabled {
		embed.Color = 0x2B2D31
		embed.Footer = &discordgo.MessageEmbedFooter{Text: "✓ Ο περιορισμός αφαιρέθηκε. Ευχαριστούμε!"}
	}
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    guardButtonLabel,
				Style:    discordgo.LinkButton,
				URL:      link,
				Disabled: disabled,
			},
		}},
	}
	return embed, components
}

func (h *Handler) sendGuardDM(s *discordgo.Session, userID, reason, link string) (store.GuardDM, error) {
	ch, err := s.UserChannelCreate(userID)
	if err != nil {
		return store.GuardDM{}, err
	}
	embed, components := guardMessage(reason, link, false)
	msg, err := s.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{embed},
		Components:      components,
		AllowedMentions: moderation.SafeMentions,
	})
	if err != nil {
		return store.GuardDM{}, err
	}
	return store.GuardDM{ChannelID: ch.ID, MessageID: msg.ID}, nil
}

// GuardLifter returns the verify.Lifter that lifts timeouts through s.
func (h *Handler) GuardLifter(s *discordgo.Session) verify.Lifter {
	return guardLifter{h: h, s: s}
}

type guardLifter struct {
	h *Handler
	s *discordgo.Session
}

func (g guardLifter) TokenActive(ctx context.Context, c *verify.Claims) (bool, error) {
	return g.h.st.GuardTokenActive(ctx, c.Subject, c.ID)
}

// Lift removes the timeout on Discord first and only then spends the link, so
// a Discord failure leaves the member a link that still works on retry.
func (g guardLifter) Lift(ctx context.Context, c *verify.Claims) error {
	h, s := g.h, g.s
	userID := c.Subject
	// One lift at a time per member, so duplicate requests queue behind the
	// first and find the link spent instead of each calling Discord.
	defer h.lockUser(userID)()

	active, err := h.st.GuardTokenActive(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	if !active {
		return verify.ErrLinkUsed
	}

	// A timeout that runs past this link's own belongs to a newer
	// restriction or a moderator, and a captcha must not undo it.
	foreign := false
	if m, merr := s.GuildMember(c.GuildID, userID, discordgo.WithContext(ctx)); merr == nil &&
		m.CommunicationDisabledUntil != nil && c.ExpiresAt != nil &&
		m.CommunicationDisabledUntil.After(c.ExpiresAt.Add(time.Minute)) {
		foreign = true
	}
	if !foreign {
		err = h.retry(ctx, "member.timeout.clear", func() error {
			return s.GuildMemberTimeout(c.GuildID, userID, nil, discordgo.WithContext(ctx))
		})
		if err != nil && !dc.IsMissingResource(err) {
			return fmt.Errorf("clear timeout: %w", err)
		}
	}

	dm, err := h.st.ConsumeGuardToken(ctx, userID, c.ID, time.Now().Add(guardExemption))
	if errors.Is(err, store.ErrNotFound) {
		// A concurrent solve of the same link got there first; the
		// timeout is lifted either way.
		return nil
	}
	if err != nil {
		return err
	}

	if dm.ChannelID != "" && dm.MessageID != "" {
		embed, components := guardMessage(c.Reason, h.cfg.PublicURL+"/verify", true)
		embeds := []*discordgo.MessageEmbed{embed}
		if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    dm.ChannelID,
			ID:         dm.MessageID,
			Embeds:     &embeds,
			Components: &components,
		}, discordgo.WithContext(ctx)); err != nil {
			h.log.Warn("media guard: disable button", "user_id", userID, "error", err)
		}
	}

	h.audit(userID, "", store.EventMediaLifted, nil, userID)
	h.log.Info("media guard: lifted", "user_id", userID)
	h.alertMods(s, "✅ Ο χρήστης <@%s> έλυσε το captcha και το timeout του αφαιρέθηκε.", userID)
	return nil
}

func newTokenID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
