package moderation

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
	"steki-bot/dc"
	"steki-bot/rpc"
	"steki-bot/store"
)

// /member ban removes someone from the server for good and closes every email
// address tied to them, so a new account cannot register with it. The Discord
// ban stops the account; the address ban stops the person.

// banReasonLimit keeps the reason inside Discord's audit-log limit.
const banReasonLimit = 400

func (h *Handler) cmdMemberBan(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := bot.OptUser(i, sub.Options, "user")
	reason := strings.TrimSpace(bot.OptString(sub.Options, "reason"))
	extra := strings.TrimSpace(bot.OptString(sub.Options, "email"))
	by := bot.ActorID(i)

	switch {
	case user.Bot:
		h.Reply(s, i, "Δεν μπορείς να αποκλείσεις bot με αυτή την εντολή.")
		return
	case user.ID == by:
		h.Reply(s, i, "Δεν μπορείς να αποκλείσεις τον εαυτό σου.")
		return
	case extra != "" && !strings.Contains(extra, "@"):
		h.Reply(s, i, fmt.Sprintf("Το `%s` δεν είναι έγκυρο email.", extra))
		return
	}

	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	if member, err := h.GuildMember(ctx, s, i.GuildID, user.ID); err == nil {
		if bot.HasAnyRole(member, []string{bot.StaffRoleID, bot.ModRoleID}) {
			h.Reply(s, i, fmt.Sprintf("Ο <@%s> ανήκει στην Ομάδα Διαχείρισης και δεν μπορεί να αποκλειστεί με αυτή την εντολή.", user.ID))
			return
		}
	} else if !dc.IsMissingResource(err) {
		h.Reply(s, i, fmt.Sprintf("Δεν μπόρεσα να ελέγξω τον <@%s>: %v", user.ID, err))
		return
	}

	defer h.LockUser(user.ID)()

	// The addresses go first: they are read from the member's records, which
	// stay put, and a failed Discord ban can then be retried without losing them.
	var banned store.BanResult
	err := h.registration.Call(ctx, rpc.PathEmailsBan,
		rpc.BanRequest{UserID: user.ID, Extra: extra, Reason: reason, By: by}, &banned)
	if err != nil {
		h.DBError(s, i, err)
		return
	}

	auditReason := "Ban από " + actorName(i)
	if reason != "" {
		auditReason = bot.TruncateRunes(reason, banReasonLimit)
	}
	err = h.Retry(ctx, "guild.ban", func() error {
		return s.GuildBanCreateWithReason(i.GuildID, user.ID, auditReason, 0, discordgo.WithContext(ctx))
	})

	h.Audit(user.ID, "", store.EventModAction, map[string]any{
		"action": "ban", "reason": reason, "emails": len(banned.Emails), "hashes": banned.Hashes,
		"discord_ban_ok": err == nil,
	}, by)

	addresses := fmt.Sprintf("%d email", len(banned.Emails))
	if banned.Hashes > 0 {
		addresses += fmt.Sprintf(" και %d παλιά hashes", banned.Hashes)
	}
	if err != nil {
		h.Reply(s, i, fmt.Sprintf(
			"⚠️ Τα email του <@%s> αποκλείστηκαν (%s) αλλά το ban στο Discord απέτυχε: %v", user.ID, addresses, err))
		return
	}
	h.AlertMods(s, "🔨 Ο <@%s> αποκλείστηκε από τον <@%s>.%s", user.ID, by, reasonLine(reason))
	h.Reply(s, i, fmt.Sprintf("🔨 Ο <@%s> αποκλείστηκε από τον server. Δεν μπορεί να ξαναγραφτεί με: %s.", user.ID, addresses))
}

func (h *Handler) cmdMemberUnban(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := bot.OptUser(i, sub.Options, "user")
	extra := strings.TrimSpace(bot.OptString(sub.Options, "email"))
	by := bot.ActorID(i)

	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	var ans rpc.UnbanAnswer
	err := h.registration.Call(ctx, rpc.PathEmailsUnban, rpc.UnbanRequest{UserID: user.ID, Email: extra}, &ans)
	if err != nil {
		h.DBError(s, i, err)
		return
	}
	lifted := ans.Lifted

	// Having no Discord ban is not an error: the command is also how a
	// moderator makes sure the account and its addresses are free again.
	discordLifted := true
	err = h.Retry(ctx, "guild.unban", func() error {
		return s.GuildBanDelete(i.GuildID, user.ID, discordgo.WithContext(ctx))
	})
	if dc.IsMissingResource(err) {
		discordLifted, err = false, nil
	}
	if err != nil {
		h.Reply(s, i, fmt.Sprintf("⚠️ Αφαιρέθηκαν %d email από τη λίστα αποκλεισμού αλλά το unban στο Discord απέτυχε: %v", lifted, err))
		return
	}
	if !discordLifted && lifted == 0 {
		h.Reply(s, i, fmt.Sprintf("Ο <@%s> δεν είναι αποκλεισμένος.", user.ID))
		return
	}

	h.Audit(user.ID, "", store.EventModAction, map[string]any{
		"action": "unban", "emails": lifted, "discord_ban": discordLifted,
	}, by)
	h.AlertMods(s, "♻️ Ο αποκλεισμός του <@%s> άρθηκε από τον <@%s>.", user.ID, by)
	h.Reply(s, i, fmt.Sprintf("♻️ Ο <@%s> δεν είναι πια αποκλεισμένος (%d email ελευθερώθηκαν).", user.ID, lifted))
}

// actorName is the moderator's name, for the server's audit log.
func actorName(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.Username
	}
	return "moderator"
}

// reasonLine is the reason as a trailing line for a moderator log post.
func reasonLine(reason string) string {
	if reason == "" {
		return ""
	}
	return "\nΛόγος: " + bot.TruncateRunes(reason, banReasonLimit)
}
