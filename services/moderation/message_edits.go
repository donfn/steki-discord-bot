package moderation

import (
	"time"

	"steki-bot/bot"

	"github.com/bwmarrin/discordgo"
)

// auditLogWindow is how recent a message-delete audit entry must be to be
// attributed to a given deletion.
const auditLogWindow = 20 * time.Second

// onMessageUpdate screens edited messages and logs edits to Steki messages
// for moderators.
//
// Screening runs on every edit, cached or not: otherwise a member could post
// something harmless and then edit a phishing link or a foreign invite into
// it. Edits are not counted towards the media guard or duplicate detection,
// which would count the same message twice.
//
// The previous content for the log comes from the message cache, so edits to
// messages sent before the bot started, or evicted from the cache, are not
// reported.
func (h *Handler) onMessageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if m.Message == nil || m.Author == nil || m.Author.Bot || m.GuildID == "" {
		return
	}
	if h.InHomeGuild(m.GuildID) {
		h.Track("discord_message_edited", bot.MessageProps(m.Message))
	}
	var before cachedMsg
	var cached bool
	if m.Content != "" {
		before, cached = h.msgs.edit(m.ID, m.Content)
	}
	if m.Content != "" && (!cached || before.content != clip(m.Content, msgCacheMaxContent)) {
		if h.linkChk.Check(s, m.Message) {
			h.blocked.Add(1)
			return
		}
		if h.InHomeGuild(m.GuildID) && h.screenInvites(s, m.Message) {
			h.blocked.Add(1)
			return
		}
	}
	if !h.InHomeGuild(m.GuildID) {
		return
	}
	if !cached || before.content == "" || m.Content == "" {
		return
	}
	if before.content == clip(m.Content, msgCacheMaxContent) {
		return
	}

	h.AlertMods(s,
		"Ο χρήστης <@%s> επεξεργάστηκε ένα μήνυμα στο <#%s>.\n*Πριν:*```%s```*Μετά:*```%s```",
		m.Author.ID, m.ChannelID, bot.Quote(before.content), bot.Quote(m.Content))
}

// onMessageDelete logs deleted Steki messages for moderators, attributing the
// deletion to whoever the audit log says removed it. Deletions by staff are
// not reported, and, as with edits, only cached messages can be.
func (h *Handler) onMessageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	// Take the message even for other guilds, so the ring does not hold on to it.
	gone, cached := h.msgs.take(m.ID)
	if !h.InHomeGuild(m.GuildID) {
		return
	}
	h.Track("discord_message_deleted", map[string]any{
		"guild_id": m.GuildID, "channel_id": m.ChannelID, "author_known": cached,
	})
	if !cached || gone.bot {
		return
	}

	ctx, cancel := h.Ctx()
	defer cancel()

	member, err := h.GuildMember(ctx, s, m.GuildID, gone.authorID)
	if err == nil && (bot.HasRole(member, bot.StaffRoleID) || bot.HasRole(member, bot.ModRoleID)) {
		return
	}

	h.AlertMods(s, "Ο χρήστης <@%s> διέγραψε αυτό το μήνυμα από το <#%s>:\n```%s```",
		h.deletedBy(s, m.GuildID, m.ChannelID, gone.authorID), m.ChannelID, bot.Quote(gone.content))
}

// deletedBy returns who deleted a message by authorID in channelID. Discord
// only writes an audit entry when someone deletes another member's message, so
// with no recent matching entry the author deleted it themselves.
func (h *Handler) deletedBy(s *discordgo.Session, guildID, channelID, authorID string) string {
	logs, err := s.GuildAuditLog(guildID, "", "", int(discordgo.AuditLogActionMessageDelete), 10)
	if err != nil {
		h.Log.Debug("read audit log failed", "error", err)
		return authorID
	}
	for _, entry := range logs.AuditLogEntries {
		if entry.TargetID != authorID {
			continue
		}
		if entry.Options != nil && entry.Options.ChannelID != channelID {
			continue
		}
		at, err := discordgo.SnowflakeTimestamp(entry.ID)
		if err != nil || time.Since(at) > auditLogWindow {
			continue
		}
		return entry.UserID
	}
	return authorID
}
