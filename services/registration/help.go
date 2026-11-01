package registration

import (
	"context"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
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
	err := h.Retry(ctx, "help.grant", func() error {
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
		h.Log.Warn("help: lookup failed", "user_id", userID, "error", err)
		return
	}
	if !has {
		return
	}
	err = h.Retry(ctx, "help.revoke", func() error {
		return s.ChannelPermissionDelete(config.SupportChannelID, userID, discordgo.WithContext(ctx))
	})
	if err != nil && !dc.IsMissingResource(err) {
		h.Log.Warn("help: revoke failed", "user_id", userID, "error", err)
		return
	}
	if err := h.st.ForgetHelpAccess(ctx, userID); err != nil {
		h.Log.Warn("help: forget grant failed", "user_id", userID, "error", err)
	}
}

// sweepHelp revokes access left behind by any path that closed a
// registration without going through revokeHelp.
func (h *Handler) sweepHelp(ctx context.Context, s *discordgo.Session) {
	stale, err := h.st.StaleHelpAccess(ctx)
	if err != nil {
		h.Log.Warn("help: sweep failed", "error", err)
		return
	}
	for _, userID := range stale {
		h.revokeHelp(ctx, s, userID)
	}
	if len(stale) > 0 {
		h.Log.Info("help: swept stale access", "members", len(stale))
	}
}

// onHelpButton lets the member into the help channel and tells them where to
// go.
func (h *Handler) onHelpButton(s *discordgo.Session, i *discordgo.InteractionCreate, reg *store.Registration) {
	ctx, cancel := h.Ctx()
	defer cancel()

	if err := h.grantHelp(ctx, s, reg.UserID); err != nil {
		h.Log.Error("help: grant failed", "user_id", reg.UserID, "error", err)
		h.RespondNow(s, i, bot.MsgGenericError)
		return
	}
	h.Audit(reg.UserID, reg.ID, store.EventHelpRequested, nil, "")
	h.UrgentMods(s, "🆘 Ο <@%s> ζήτησε βοήθεια με την εγγραφή του στο <#%s>.", reg.UserID, config.SupportChannelID)
	h.RespondNow(s, i, "Σου άνοιξα το <#"+config.SupportChannelID+">. Γράψε εκεί τι πρόβλημα έχεις "+
		"και κάποιος από την ομάδα θα σου απαντήσει.\n"+
		"-# Το κανάλι θα κρυφτεί ξανά όταν τελειώσει η εγγραφή σου.")
}
