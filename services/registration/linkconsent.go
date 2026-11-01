package registration

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
	"steki-bot/brand"
	"steki-bot/mail"
	"steki-bot/store"
	"steki-bot/tokens"
)

// universityFor maps an eligible address to its key in brand.Universities.
func universityFor(email string) string {
	if csdEmailRegex.MatchString(email) {
		return "uoc"
	}
	return "hmu"
}

// identity is how the email and the consent page name the account being
// linked. The display name is chosen freely and can mimic someone else, so it
// is shown with the account's unique @username, with control and bidi
// characters stripped and the length capped.
func identity(u *discordgo.User) string {
	clean := func(s string) string {
		return bot.TruncateRunes(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, s), 32)
	}
	name, handle := clean(bot.DisplayName(u)), clean(u.Username)
	if name == handle {
		return "@" + handle
	}
	return name + " (@" + handle + ")"
}

// linkEmail describes the registration email for one link.
func (h *Handler) linkEmail(u *discordgo.User, email, token string) mail.Link {
	uni := brand.Universities[universityFor(email)]
	return mail.Link{
		URL:        h.cfg.PublicURL + "/link?t=" + url.QueryEscape(token),
		Username:   identity(u),
		AvatarURL:  u.AvatarURL("128"),
		Email:      email,
		UniName:    uni.Name,
		UniLogoURL: h.cfg.PublicURL + "/verify/uni/" + uni.Logo,
		LogoURL:    h.cfg.PublicURL + "/verify/logo.png",
		Logo:       brand.Logo(),
		UniLogo:    brand.UniLogo(uni.Logo),
		ValidFor:   linkTTL,
	}
}

type linkConsenter struct {
	h *Handler
	s *discordgo.Session
}

// pendingRegistration returns the member's open registration when c is the
// link it is waiting on: the newest one sent, not yet expired.
func (l linkConsenter) pendingRegistration(ctx context.Context, c *tokens.Claims) (*store.Registration, error) {
	reg, err := l.h.st.OpenRegistration(ctx, c.Subject)
	if errors.Is(err, store.ErrNotFound) {
		return nil, tokens.ErrLinkUsed
	}
	if err != nil {
		return nil, err
	}
	if reg.Step != store.StepAwaitingCode || reg.Email == nil ||
		!reg.CodeValid(time.Now()) || *reg.Code != c.ID {
		return nil, tokens.ErrLinkUsed
	}
	return reg, nil
}

func (l linkConsenter) Pending(ctx context.Context, c *tokens.Claims) (*brand.Consent, error) {
	reg, err := l.pendingRegistration(ctx, c)
	if err != nil {
		return nil, err
	}
	user, err := l.user(ctx, reg)
	if err != nil {
		return nil, err
	}
	return &brand.Consent{
		Username:   identity(user),
		AvatarURL:  user.AvatarURL("128"),
		Email:      *reg.Email,
		University: universityFor(*reg.Email),
	}, nil
}

// user looks the member up in the gateway cache, falling back to the API.
func (l linkConsenter) user(ctx context.Context, reg *store.Registration) (*discordgo.User, error) {
	if m, err := l.s.State.Member(reg.GuildID, reg.UserID); err == nil && m.User != nil {
		return m.User, nil
	}
	return l.s.User(reg.UserID, discordgo.WithContext(ctx))
}

// Accept binds the address and grants the role, exactly as a correct code
// used to. The member's lock is held throughout, so a second tab accepting
// the same link, or a message in the channel, waits and then finds the
// registration already closed.
func (l linkConsenter) Accept(ctx context.Context, c *tokens.Claims) error {
	h := l.h
	defer h.LockUser(c.Subject)()

	reg, err := l.pendingRegistration(ctx, c)
	if err != nil {
		return err
	}
	channelID := ""
	if reg.ChannelID != nil {
		channelID = *reg.ChannelID
	}
	err = h.completeVerification(ctx, l.s, reg.GuildID, channelID, reg)
	if errors.Is(err, store.ErrEmailTaken) {
		return tokens.ErrEmailTaken
	}
	return err
}
