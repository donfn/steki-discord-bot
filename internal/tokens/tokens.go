// Package tokens issues and checks the signed links the services hand out.
//
// Two keys sign them. The links the registration and moderation services send
// (the email link, the media guard's captcha link) use a key derived from
// INTERNAL_SECRET, which those services hold. Everything only the web service
// issues (material pages, sockets, the MCP guide) uses JWT_SECRET, which it alone
// holds, so a compromised registration or moderation service cannot forge those.
package tokens

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "steki-bot"

// Reasons a member can be restricted, carried in the link so the page can
// explain which one applied. An empty reason is a media burst, which is what
// links issued before reasons existed were for.
const (
	ReasonMedia     = "media"
	ReasonDuplicate = "duplicate"
	ReasonSpam      = "spam"
	ReasonManual    = "manual"

	// ReasonLink marks a registration email link rather than a restriction.
	// The two pages each refuse the other's tokens.
	ReasonLink = "link"

	// ReasonMaterials marks a link to a course channel's material page. Only
	// that page accepts it.
	ReasonMaterials = "materials"

	// ReasonMCPGuide marks a link to the MCP setup page. Only that page
	// accepts it.
	ReasonMCPGuide = "mcpguide"

	// ReasonSocket marks a token that opens the material page's socket, for
	// one member and one course channel. Nothing else accepts it.
	ReasonSocket = "socket"
)

// Claims identify one restriction: the member (Subject), the guild they were
// timed out in, why, and the link's own ID, which the database checks so each
// link works once.
type Claims struct {
	GuildID string `json:"gid"`
	Reason  string `json:"rsn,omitempty"`
	// ChannelID is the course channel a material page link is for.
	ChannelID string `json:"cid,omitempty"`
	// Viewer, on a material page link, is the staff member opening the page as
	// its member (/materials view-as). Such a page only shows: it saves
	// nothing, files no reports and is not counted as present.
	Viewer string `json:"vw,omitempty"`
	jwt.RegisteredClaims
}

// Profile is how the material page shows the member it belongs to.
type Profile struct {
	// Name is their server nickname or display name.
	Name string
	// Username is their Discord username.
	Username string
	// Avatar is their avatar's URL on Discord's CDN.
	Avatar string
	// Viewer is the staff member viewing as them, from the link.
	Viewer string
}

// ErrLinkUsed is returned when a link was already used or has been replaced by
// a newer one.
var ErrLinkUsed = errors.New("verify: link already used")

// ErrEmailTaken is returned when the address was bound to another account after
// the email was sent.
var ErrEmailTaken = errors.New("verify: address already in use")

// Tokens issues and checks verification links.
type Tokens struct {
	// Secret signs what only the web service issues; empty elsewhere.
	Secret []byte
	// page signs the links other services issue.
	page []byte

	spentMu sync.Mutex
	spent   map[string]time.Time
}

// New returns the web service's Tokens, able to check every kind of link.
func New(jwtSecret, internalSecret string) *Tokens {
	return &Tokens{Secret: []byte(jwtSecret), page: pageKey(internalSecret), spent: make(map[string]time.Time)}
}

// NewIssuer returns the Tokens of a service that only issues email and guard
// links.
func NewIssuer(internalSecret string) *Tokens {
	return &Tokens{page: pageKey(internalSecret), spent: make(map[string]time.Time)}
}

func pageKey(internalSecret string) []byte {
	mac := hmac.New(sha256.New, []byte(internalSecret))
	mac.Write([]byte("steki page tokens v1"))
	return mac.Sum(nil)
}

// pageReason reports whether a reason belongs to a link signed with the page key.
func pageReason(reason string) bool {
	switch reason {
	case "", ReasonMedia, ReasonDuplicate, ReasonSpam, ReasonManual, ReasonLink:
		return true
	}
	return false
}

// Issue signs a link for one restriction that stops working at expires.
func (t *Tokens) Issue(userID, guildID, reason, tokenID string, expires time.Time) (string, error) {
	claims := Claims{
		GuildID: guildID,
		Reason:  reason,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   userID,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.page)
}

// IssueMaterials signs a link to the material page of one course channel.
func (t *Tokens) IssueMaterials(userID, guildID, channelID, tokenID, viewer string, expires time.Time) (string, error) {
	claims := Claims{
		GuildID:   guildID,
		Reason:    ReasonMaterials,
		ChannelID: channelID,
		Viewer:    viewer,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   userID,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.Secret)
}

// IssueSocket signs a token that opens the material page's socket once.
func (t *Tokens) IssueSocket(userID, guildID, channelID, tokenID string, expires time.Time) (string, error) {
	claims := Claims{
		GuildID:   guildID,
		Reason:    ReasonSocket,
		ChannelID: channelID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   userID,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.Secret)
}

// IssueGuide signs a link to the MCP setup page, shared by everyone.
func (t *Tokens) IssueGuide(userID, guildID, tokenID string, expires time.Time) (string, error) {
	claims := Claims{
		GuildID: guildID,
		Reason:  ReasonMCPGuide,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   userID,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.Secret)
}

// Parse verifies a link's signature and expiry and returns its claims.
func (t *Tokens) Parse(raw string) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(raw, &c, t.keyFor,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	if c.Subject == "" || c.ID == "" || c.GuildID == "" {
		return nil, errors.New("verify: token is missing claims")
	}
	return &c, nil
}

// keyFor picks the keys a token may be signed with by its reason, so a holder of
// the page key cannot forge any other kind. Links issued before the keys were
// split carry JWT_SECRET's signature, which stays acceptable: only the web
// service, which holds it, verifies these.
func (t *Tokens) keyFor(tok *jwt.Token) (any, error) {
	c, ok := tok.Claims.(*Claims)
	if !ok {
		return nil, errors.New("verify: unexpected claims")
	}
	keys := jwt.VerificationKeySet{}
	if pageReason(c.Reason) && len(t.page) > 0 {
		keys.Keys = append(keys.Keys, t.page)
	}
	if len(t.Secret) > 0 {
		keys.Keys = append(keys.Keys, t.Secret)
	}
	if len(keys.Keys) == 0 {
		return nil, errors.New("verify: no key for this token")
	}
	return keys, nil
}

// IssueLink signs a registration email link for one registration attempt.
func (t *Tokens) IssueLink(userID, guildID, linkID string, expires time.Time) (string, error) {
	return t.Issue(userID, guildID, ReasonLink, linkID, expires)
}
