package events

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/moderation"
)

var (
	hmuEmailRegex = regexp.MustCompile(`^th[0-9]+@edu\.hmu\.gr$`)
	csdEmailRegex = regexp.MustCompile(`^csd[0-9]+@csd\.uoc\.gr$`)
)

// generateCode returns a six-digit verification code from a cryptographically
// secure source. The previous implementation used math/rand, whose output is
// predictable from a few observed codes.
func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("generate code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// classifyEmail maps a submitted address to the role it earns, reporting
// whether the address is acceptable at all.
func classifyEmail(email string) (roleID string, ok bool) {
	switch {
	case hmuEmailRegex.MatchString(email):
		return hmuRoleID, true
	case csdEmailRegex.MatchString(email):
		return csdRoleID, true
	default:
		return "", false
	}
}

func hasRole(member *discordgo.Member, roleID string) bool {
	return member != nil && slices.Contains(member.Roles, roleID)
}

func hasAnyRole(member *discordgo.Member, roleIDs []string) bool {
	if member == nil {
		return false
	}
	for _, want := range roleIDs {
		if slices.Contains(member.Roles, want) {
			return true
		}
	}
	return false
}

func registerChannelName(userID string) string {
	return registerChannelPrefix + userID
}

// userIDFromChannelName extracts the member ID from a register-<id> channel
// name, reporting whether the name had that shape.
func userIDFromChannelName(name string) (string, bool) {
	id, found := strings.CutPrefix(name, registerChannelPrefix)
	if !found || id == "" {
		return "", false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return id, true
}

// isRegisterChannel reports whether a channel is somebody's register channel.
func isRegisterChannel(ch *discordgo.Channel) bool {
	if ch == nil {
		return false
	}
	_, ok := userIDFromChannelName(ch.Name)
	return ok
}

// truncate shortens s to at most limit bytes, cutting on a rune boundary so
// Greek text never ends in half a character, and marks the cut.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const marker = "\n… (συντομεύτηκε)"
	cut := max(limit-len(marker), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// quote makes user-supplied text safe to embed in a ``` code block, where a
// stray backtick run would otherwise end the block early.
func quote(s string) string {
	return strings.ReplaceAll(s, "`", "ˋ")
}

// channel resolves a channel, preferring the state cache.
func (h *Handler) channel(ctx context.Context, s *discordgo.Session, channelID string) (*discordgo.Channel, error) {
	if ch, err := s.State.Channel(channelID); err == nil {
		return ch, nil
	}
	return s.Channel(channelID, discordgo.WithContext(ctx))
}

// channelExists reports whether a channel is still there. A transport or
// permission failure is returned as an error rather than as "gone", so callers
// never recreate a channel just because Discord was briefly unreachable.
func (h *Handler) channelExists(ctx context.Context, s *discordgo.Session, channelID string) (bool, error) {
	_, err := h.channel(ctx, s, channelID)
	switch {
	case err == nil:
		return true, nil
	case dc.IsMissingResource(err):
		return false, nil
	default:
		return false, err
	}
}

// deleteChannel removes a channel, treating an already-deleted channel as
// success so cleanup paths stay idempotent.
func (h *Handler) deleteChannel(ctx context.Context, s *discordgo.Session, channelID string) error {
	err := h.retry(ctx, "channel.delete", func() error {
		_, derr := s.ChannelDelete(channelID, discordgo.WithContext(ctx))
		return derr
	})
	if err != nil && dc.IsMissingResource(err) {
		return nil
	}
	return err
}

// grantRole adds a role to a member, with retries.
func (h *Handler) grantRole(ctx context.Context, s *discordgo.Session, guildID, userID, roleID string) error {
	return h.retry(ctx, "member.role.add", func() error {
		return s.GuildMemberRoleAdd(guildID, userID, roleID, discordgo.WithContext(ctx))
	})
}

// send posts a message, logging failures rather than propagating them: a
// member-facing message that fails should not abort the surrounding flow.
func (h *Handler) send(s *discordgo.Session, channelID, content string) *discordgo.Message {
	msg, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content, AllowedMentions: moderation.SafeMentions,
	})
	if err != nil {
		if !dc.IsMissingResource(err) {
			h.log.Warn("send message failed", "channel_id", channelID, "error", err)
		}
		return nil
	}
	return msg
}

// guildMember fetches a member, preferring the state cache.
func (h *Handler) guildMember(ctx context.Context, s *discordgo.Session, guildID, userID string) (*discordgo.Member, error) {
	if m, err := s.State.Member(guildID, userID); err == nil {
		return m, nil
	}
	var member *discordgo.Member
	err := h.retry(ctx, "guild.member", func() error {
		m, merr := s.GuildMember(guildID, userID, discordgo.WithContext(ctx))
		if merr != nil {
			return merr
		}
		member = m
		return nil
	})
	return member, err
}
