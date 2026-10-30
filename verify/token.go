// Package verify serves the bot's two public pages. The media guard page lets
// a member who was timed out for posting attachments or links in quick
// succession solve an ALTCHA proof-of-work challenge and have the timeout
// lifted. The link page is where the registration email points: the member
// confirms that their Discord account and academic address belong together,
// and the bot lets them in.
package verify

import (
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

// Tokens issues and checks verification links.
type Tokens struct {
	secret []byte

	spentMu sync.Mutex
	spent   map[string]time.Time
}

// NewTokens returns a signer for the given secret.
func NewTokens(secret string) *Tokens {
	return &Tokens{secret: []byte(secret), spent: make(map[string]time.Time)}
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
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
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
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
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
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
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
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

// Parse verifies a link's signature and expiry and returns its claims.
func (t *Tokens) Parse(raw string) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
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
