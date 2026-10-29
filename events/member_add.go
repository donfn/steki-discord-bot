package events

import (
	"errors"
	"fmt"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
	"steki-bot/dc"
	"steki-bot/store"
)

// onMemberAdd records a join, greets the member and gives them a registration
// channel.
func (h *Handler) onMemberAdd(s *discordgo.Session, m *discordgo.GuildMemberAdd) {
	if m.User == nil || m.User.Bot || !h.inHomeGuild(m.GuildID) {
		return
	}

	ctx, cancel := h.ctx()
	err := h.st.UpsertMemberJoin(ctx, m.User.ID, m.User.Username)
	cancel()
	if err != nil {
		// Carry on: a missing join counter must not cost the member their
		// registration channel.
		h.log.Error("record member join failed", "user_id", m.User.ID, "error", err)
	}
	h.audit(m.User.ID, "", store.EventJoined, map[string]any{"username": m.User.Username}, "")

	h.ensureRegisterChannel(s, m.GuildID, m.User)
}

// ensureRegisterChannel makes sure an unverified member has an open
// registration with a live channel. It is the one path shared by the gateway
// join and the reconciler, which is what lets a join missed during a gateway
// outage be recovered later without duplicating any logic.
//
// It is idempotent: the registration row is created under a partial unique
// index, an existing channel is reused, and the per-member lock keeps a
// redelivered GUILD_MEMBER_ADD and a reconciler sweep from both creating one.
func (h *Handler) ensureRegisterChannel(s *discordgo.Session, guildID string, user *discordgo.User) {
	defer h.lockUser(user.ID)()

	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	if err := h.st.EnsureMember(ctx, user.ID, user.Username); err != nil {
		h.log.Error("ensure member failed", "user_id", user.ID, "error", err)
		h.alertMods(s, "⚠️ Δεν μπόρεσα να ανοίξω εγγραφή για τον <@%s>: ```%v```", user.ID, err)
		return
	}
	reg, created, err := h.st.EnsureOpenRegistration(ctx, user.ID, guildID)
	if err != nil {
		h.log.Error("ensure registration failed", "user_id", user.ID, "error", err)
		h.alertMods(s, "⚠️ Δεν μπόρεσα να ανοίξω εγγραφή για τον <@%s>: ```%v```", user.ID, err)
		return
	}

	// An open registration that still points at a live channel needs nothing.
	if !created && reg.ChannelID != nil {
		exists, err := h.channelExists(ctx, s, *reg.ChannelID)
		if err != nil {
			h.log.Error("cannot confirm register channel, leaving it for the reconciler",
				"user_id", user.ID, "channel_id", *reg.ChannelID, "error", err)
			return
		}
		if exists {
			return
		}
		h.log.Info("registration channel vanished, recreating",
			"user_id", user.ID, "channel_id", *reg.ChannelID)
	}

	ch, categoryID, err := h.placer.Create(ctx, s, guildID, registerChannelData(guildID, user.ID))
	if err != nil {
		h.reportChannelFailure(s, user, reg.ID, err)
		return
	}

	if err := h.st.AttachChannel(ctx, reg.ID, ch.ID, categoryID); err != nil {
		// The reconciler adopts a register channel its row lost track of.
		h.log.Error("attach channel failed", "user_id", user.ID, "channel_id", ch.ID, "error", err)
	}
	h.audit(user.ID, reg.ID, store.EventChannelCreated,
		map[string]any{"channel_id": ch.ID, "category_id": categoryID}, "")

	h.sendWelcome(s, ch.ID, user.ID)
}

// registerChannelData describes a member's registration channel: hidden from
// everyone except the member and the moderators who watch registrations.
func registerChannelData(guildID, userID string) discordgo.GuildChannelCreateData {
	return discordgo.GuildChannelCreateData{
		Name:  registerChannelName(userID),
		Type:  discordgo.ChannelTypeGuildText,
		Topic: "Ολοκλήρωσε την εγγραφή σου για να μπορέσεις να δείς όλα τα κανάλια στο Steki!",
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:   guildID, // @everyone
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionViewChannel,
			},
			memberOverwrite(userID),
			{
				ID:    modViewRoleID,
				Type:  discordgo.PermissionOverwriteTypeRole,
				Allow: discordgo.PermissionViewChannel,
			},
		},
	}
}

// Members can read their register channel and react in it (the interest
// roles are picked by reaction) but not write: they answer through the
// buttons and the email form. Moderators keep whatever their roles give them.
const (
	registerMemberAllow = discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory |
		discordgo.PermissionAddReactions
	registerMemberDeny = discordgo.PermissionSendMessages | discordgo.PermissionSendMessagesInThreads |
		discordgo.PermissionCreatePublicThreads | discordgo.PermissionCreatePrivateThreads
)

func memberOverwrite(userID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:    userID,
		Type:  discordgo.PermissionOverwriteTypeMember,
		Allow: registerMemberAllow,
		Deny:  registerMemberDeny,
	}
}

// reportChannelFailure makes a failed channel creation loud. The production
// incident this replaces logged one line to stdout and returned, leaving the
// member with no channel and nobody aware of it.
func (h *Handler) reportChannelFailure(s *discordgo.Session, user *discordgo.User, regID string, err error) {
	h.log.Error("create register channel failed",
		"user_id", user.ID, "username", user.Username,
		"api_code", dc.APICode(err), "http_status", dc.RESTCode(err), "error", err)

	h.audit(user.ID, regID, store.EventChannelCreateFailed,
		map[string]any{"error": err.Error(), "api_code": dc.APICode(err)}, "")

	detail := fmt.Sprintf("```%v```", err)
	if errors.Is(err, dc.ErrGuildChannelLimit) {
		h.alertMods(s,
			"🚨 <@&%s> Ο σέρβερ έφτασε το όριο καναλιών του Discord. "+
				"Ο χρήστης **%s** (<@%s>) **δεν** πήρε κανάλι εγγραφής.\n"+
				"Χρειάζεται να διαγραφούν κανάλια χειροκίνητα.\n%s",
			staffRoleID, user.Username, user.ID, detail)
		return
	}
	h.alertMods(s,
		"🚨 <@&%s> Απέτυχε η δημιουργία καναλιού εγγραφής για τον **%s** (<@%s>).\n"+
			"Ο χρήστης **δεν** μπορεί να γραφτεί μέχρι να λυθεί.\n%s",
		staffRoleID, user.Username, user.ID, detail)
}

// sendWelcome points the member at their channel and posts the card there.
func (h *Handler) sendWelcome(s *discordgo.Session, channelID, userID string) {
	announcement := fmt.Sprintf("👋 Καλώς ήρθες, <@%s>! Για να μπεις στο Steki, πήγαινε εδώ: <#%s>", userID, channelID)
	h.send(s, config.WelcomeChannelID, announcement)
	if dm, err := s.UserChannelCreate(userID); err == nil {
		h.send(s, dm.ID, announcement)
	}
	h.postFlowIntro(s, channelID, userID)
}

// postFlowIntro posts the registration card. Shared by a fresh channel and a
// restarted one.
func (h *Handler) postFlowIntro(s *discordgo.Session, channelID, userID string) {
	h.postCard(s, channelID, card{userID: userID, state: cardAsk})
}
