// Package moderation holds the bot's moderator-facing plumbing: the log
// channel it reports to, and the blacklisted-link filter.
package moderation

import (
	"log/slog"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// discordMessageLimit is the maximum length of a single Discord message.
const discordMessageLimit = 2000

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

// Post sends a message, splitting anything over Discord's length limit so a
// long error dump is truncated rather than dropped entirely.
func (b *Log) Post(s *discordgo.Session, message string) {
	for _, chunk := range splitMessage(message) {
		if _, err := s.ChannelMessageSend(b.channelID, chunk); err != nil {
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
		if len(message) <= discordMessageLimit {
			out = append(out, message)
			break
		}
		cut := discordMessageLimit
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		out = append(out, message[:cut])
		message = message[cut:]
	}
	return out
}
