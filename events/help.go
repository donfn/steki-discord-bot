package events

import (
	"context"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
	"steki-bot/dc"
	"steki-bot/store"
)

// The help channel is hidden from everyone. A member stuck in registration
// presses "Χρειάζομαι βοήθεια" and gets a permission overwrite of their own,
// which the bot takes away again as soon as the registration is completed,
// restarted or closed for any reason.

const helpButtonID = "reg:help"

const helpAllow = discordgo.PermissionViewChannel | discordgo.PermissionSendMessages |
	discordgo.PermissionReadMessageHistory | discordgo.PermissionAttachFiles

// grantHelp lets the member into the help channel.
func (h *Handler) grantHelp(ctx context.Context, s *discordgo.Session, userID string) error {
	err := h.retry(ctx, "help.grant", func() error {
		return s.ChannelPermissionSet(config.SupportChannelID, userID,
			discordgo.PermissionOverwriteTypeMember, helpAllow, 0, discordgo.WithContext(ctx))
	})
	if err != nil {
		return err
	}
	return h.st.RecordHelpAccess(ctx, userID)
}

// revokeHelp takes the member out of the help channel, if the bot let them
// in. An overwrite a moderator gave by hand is left alone.
func (h *Handler) revokeHelp(ctx context.Context, s *discordgo.Session, userID string) {
	has, err := h.st.HasHelpAccess(ctx, userID)
	if err != nil {
		h.log.Warn("help: lookup failed", "user_id", userID, "error", err)
		return
	}
	if !has {
		return
	}
	err = h.retry(ctx, "help.revoke", func() error {
		return s.ChannelPermissionDelete(config.SupportChannelID, userID, discordgo.WithContext(ctx))
	})
	if err != nil && !dc.IsMissingResource(err) {
		h.log.Warn("help: revoke failed", "user_id", userID, "error", err)
		return
	}
	if err := h.st.ForgetHelpAccess(ctx, userID); err != nil {
		h.log.Warn("help: forget grant failed", "user_id", userID, "error", err)
	}
}

// sweepHelp revokes access left behind by any path that closed a
// registration without going through revokeHelp.
func (h *Handler) sweepHelp(ctx context.Context, s *discordgo.Session) {
	stale, err := h.st.StaleHelpAccess(ctx)
	if err != nil {
		h.log.Warn("help: sweep failed", "error", err)
		return
	}
	for _, userID := range stale {
		h.revokeHelp(ctx, s, userID)
	}
	if len(stale) > 0 {
		h.log.Info("help: swept stale access", "members", len(stale))
	}
}

// onHelpButton lets the member into the help channel and tells them where to
// go.
func (h *Handler) onHelpButton(s *discordgo.Session, i *discordgo.InteractionCreate, reg *store.Registration) {
	ctx, cancel := h.ctx()
	defer cancel()

	if err := h.grantHelp(ctx, s, reg.UserID); err != nil {
		h.log.Error("help: grant failed", "user_id", reg.UserID, "error", err)
		h.respondNow(s, i, msgGenericError)
		return
	}
	h.audit(reg.UserID, reg.ID, store.EventHelpRequested, nil, "")
	h.alertMods(s, "🆘 Ο <@%s> ζήτησε βοήθεια με την εγγραφή του στο <#%s>.", reg.UserID, config.SupportChannelID)
	h.respondNow(s, i, "Σε έβαλα στο <#"+config.SupportChannelID+">. Γράψε εκεί τι πρόβλημα έχεις "+
		"και κάποιος από την ομάδα θα σου απαντήσει.\n"+
		"-# Το κανάλι θα κρυφτεί ξανά όταν τελειώσει η εγγραφή σου.")
}
