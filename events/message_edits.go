package events

import (
	"time"

	"github.com/bwmarrin/discordgo"
)

// auditLogWindow is how recent a message-delete audit entry must be to be
// attributed to a given deletion.
const auditLogWindow = 20 * time.Second

// onMessageUpdate logs edits to Steki messages for moderators. The previous
// content comes from the state cache, so edits to messages sent before the bot
// started, or evicted from the cache, are not reported.
func (h *Handler) onMessageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if !h.inHomeGuild(m.GuildID) || m.Author == nil || m.Author.Bot {
		return
	}
	if m.BeforeUpdate == nil || m.BeforeUpdate.Content == "" || m.Content == "" {
		return
	}
	if m.BeforeUpdate.Content == m.Content {
		return
	}

	h.alertMods(s,
		"Ο χρήστης <@%s> επεξεργάστηκε στο κανάλι <#%s> αυτό το μήνυμα:\n```%s```*και η νέα έκδοσή του είναι:*```%s```",
		m.Author.ID, m.ChannelID, quote(m.BeforeUpdate.Content), quote(m.Content))
}

// onMessageDelete logs deleted Steki messages for moderators, attributing the
// deletion to whoever the audit log says removed it. Deletions by staff are
// not reported, and, as with edits, only cached messages can be.
func (h *Handler) onMessageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	if !h.inHomeGuild(m.GuildID) || m.BeforeDelete == nil || m.BeforeDelete.Author == nil {
		return
	}
	author := m.BeforeDelete.Author
	if author.Bot {
		return
	}

	ctx, cancel := h.ctx()
	defer cancel()

	member, err := h.guildMember(ctx, s, m.GuildID, author.ID)
	if err == nil && (hasRole(member, staffRoleID) || hasRole(member, modRoleID)) {
		return
	}

	h.alertMods(s, "Ο χρήστης <@%s> διέγραψε από το κανάλι <#%s> αυτό το μήνυμα:\n```%s```",
		h.deletedBy(s, m.GuildID, m.ChannelID, author.ID), m.ChannelID, quote(m.BeforeDelete.Content))
}

// deletedBy returns who deleted a message by authorID in channelID. Discord
// only writes an audit entry when someone deletes another member's message, so
// with no recent matching entry the author deleted it themselves.
func (h *Handler) deletedBy(s *discordgo.Session, guildID, channelID, authorID string) string {
	logs, err := s.GuildAuditLog(guildID, "", "", int(discordgo.AuditLogActionMessageDelete), 10)
	if err != nil {
		h.log.Debug("read audit log failed", "error", err)
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
