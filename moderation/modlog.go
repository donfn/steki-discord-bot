// Package moderation holds the bot's moderator-facing plumbing: the log
// channel it reports to, and the blacklisted-link filter.
package moderation

import (
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// discordMessageLimit is the maximum length of a single Discord message.
const discordMessageLimit = 2000

// chunkLimit leaves room for the fence markers balanceFences may add.
const chunkLimit = discordMessageLimit - 8

// Log posts operational messages to the moderator log channel.
//
// This is the bot's only channel for telling moderators that something went
// wrong, so a failure to deliver is itself logged rather than discarded.
type Log struct {
	channelID string
	log       *slog.Logger
}

// NewLog returns a Log that posts to channelID.
func NewLog(channelID string, logger *slog.Logger) *Log {
	return &Log{channelID: channelID, log: logger}
}

// Post sends a message silently: it shows in the channel but notifies no
// one. Anything over Discord's length limit is split, so a long error dump is
// truncated rather than dropped entirely.
func (b *Log) Post(s *discordgo.Session, message string) {
	b.send(s, message, discordgo.MessageFlagsSuppressNotifications)
}

// Alert sends a message with a notification, for posts an admin has to act on.
func (b *Log) Alert(s *discordgo.Session, message string) {
	b.send(s, message, 0)
}

func (b *Log) send(s *discordgo.Session, message string, flags discordgo.MessageFlags) {
	for _, chunk := range splitMessage(message) {
		if _, err := s.ChannelMessageSendComplex(b.channelID, &discordgo.MessageSend{
			Content: chunk, AllowedMentions: SafeMentions, Flags: flags,
		}); err != nil {
			b.log.Error("mod log delivery failed",
				"channel_id", b.channelID, "error", err)
			return
		}
	}
}

// splitMessage breaks a string into Discord-sized pieces on rune boundaries.
func splitMessage(message string) []string {
	if len(message) <= discordMessageLimit {
		return []string{message}
	}

	var out []string
	for len(message) > 0 {
		if len(message) <= chunkLimit {
			out = append(out, message)
			break
		}
		cut := chunkLimit
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		out = append(out, message[:cut])
		message = message[cut:]
	}
	return balanceFences(out)
}

// balanceFences closes a ``` block left open at the end of a chunk and
// reopens it at the start of the next, so member text that was quoted in a
// code block never spills out as live markdown, mentions or links.
func balanceFences(chunks []string) []string {
	open := false
	for i, c := range chunks {
		if open {
			c = "```" + c
		}
		if strings.Count(c, "```")%2 == 1 {
			open = true
			c += "```"
		} else {
			open = false
		}
		chunks[i] = c
	}
	return chunks
}
