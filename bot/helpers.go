package bot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/moderation"
)

func HasRole(member *discordgo.Member, roleID string) bool {
	return member != nil && slices.Contains(member.Roles, roleID)
}

func HasAnyRole(member *discordgo.Member, roleIDs []string) bool {
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

func RegisterChannelName(userID string) string {
	return RegisterChannelPrefix + userID
}

// userIDFromChannelName extracts the member ID from a register-<id> channel
// name, reporting whether the name had that shape.
func UserIDFromChannelName(name string) (string, bool) {
	id, found := strings.CutPrefix(name, RegisterChannelPrefix)
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
func IsRegisterChannel(ch *discordgo.Channel) bool {
	if ch == nil || ch.Type != discordgo.ChannelTypeGuildText {
		return false
	}
	_, ok := UserIDFromChannelName(ch.Name)
	return ok
}

// truncate shortens s to at most limit bytes, cutting on a rune boundary so
// Greek text never ends in half a character, and marks the cut.
func Truncate(s string, limit int) string {
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

// cleanLine turns member text into one plain line: whitespace collapsed to
// single spaces, and control and format characters (zero-width, bidirectional
// overrides) dropped, so the text cannot hide or reorder what surrounds it.
func CleanLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || (unicode.IsControl(r) && !unicode.IsSpace(r)) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// quote makes user-supplied text safe to embed in a ``` code block, where a
// stray backtick run would otherwise end the block early.
func Quote(s string) string {
	return moderation.Quote(s)
}

// channel resolves a channel, preferring the state cache.
func (h *Core) Channel(ctx context.Context, s *discordgo.Session, channelID string) (*discordgo.Channel, error) {
	if ch, err := s.State.Channel(channelID); err == nil {
		return ch, nil
	}
	return s.Channel(channelID, discordgo.WithContext(ctx))
}

// channelExists reports whether a channel is still there. A transport or
// permission failure is returned as an error rather than as "gone", so callers
// never recreate a channel just because Discord was briefly unreachable.
func (h *Core) ChannelExists(ctx context.Context, s *discordgo.Session, channelID string) (bool, error) {
	_, err := h.Channel(ctx, s, channelID)
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
func (h *Core) DeleteChannel(ctx context.Context, s *discordgo.Session, channelID string) error {
	err := h.Retry(ctx, "channel.delete", func() error {
		_, derr := s.ChannelDelete(channelID, discordgo.WithContext(ctx))
		return derr
	})
	if err != nil && dc.IsMissingResource(err) {
		return nil
	}
	return err
}

// grantRole adds a role to a member, with retries.
func (h *Core) GrantRole(ctx context.Context, s *discordgo.Session, guildID, userID, roleID string) error {
	return h.Retry(ctx, "member.role.add", func() error {
		return s.GuildMemberRoleAdd(guildID, userID, roleID, discordgo.WithContext(ctx))
	})
}

// send posts a message, logging failures rather than propagating them: a
// member-facing message that fails should not abort the surrounding flow.
func (h *Core) Send(s *discordgo.Session, channelID, content string) *discordgo.Message {
	msg, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content, AllowedMentions: moderation.SafeMentions,
	})
	if err != nil {
		if !dc.IsMissingResource(err) {
			h.Log.Warn("send message failed", "channel_id", channelID, "error", err)
		}
		return nil
	}
	return msg
}

// guildMember fetches a member, preferring the state cache.
func (h *Core) GuildMember(ctx context.Context, s *discordgo.Session, guildID, userID string) (*discordgo.Member, error) {
	if m, err := s.State.Member(guildID, userID); err == nil {
		return m, nil
	}
	var member *discordgo.Member
	err := h.Retry(ctx, "guild.member", func() error {
		m, merr := s.GuildMember(guildID, userID, discordgo.WithContext(ctx))
		if merr != nil {
			return merr
		}
		member = m
		return nil
	})
	return member, err
}

// truncateRunes cuts s to at most n runes.
func TruncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// interactionUser is whoever clicked or submitted.
func InteractionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

// FormatEventCounts renders one service's audit counts for /bot stats.
func FormatEventCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	var b strings.Builder
	b.WriteString("\n**Τελευταίο 24ωρο**\n")
	for _, k := range kinds {
		fmt.Fprintf(&b, "• `%s`: %d\n", k, counts[k])
	}
	return b.String()
}

// DisplayName is the name a member recognises as theirs: the global display
// name when they set one, otherwise the username.
func DisplayName(u *discordgo.User) string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// ErrGuildUnknown is returned before the Steki guild has been seen on the gateway.
var ErrGuildUnknown = errors.New("the Steki guild is not known yet")
