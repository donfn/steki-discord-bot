// Package verify serves the media guard's verification page: a member who was
// timed out for posting attachments or links in quick succession opens the
// link from their DM, solves an ALTCHA proof-of-work challenge, and the bot
// lifts the timeout.
package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "steki-bot"

// Claims identify one restriction: the member (Subject), the guild they were
// timed out in, and the link's own ID, which the database checks so each link
// works once.
type Claims struct {
	GuildID string `json:"gid"`
	jwt.RegisteredClaims
}

// Tokens issues and checks verification links.
type Tokens struct {
	secret []byte
}

// NewTokens returns a signer for the given secret.
func NewTokens(secret string) *Tokens {
	return &Tokens{secret: []byte(secret)}
}

// Issue signs a link for one restriction that stops working at expires.
func (t *Tokens) Issue(userID, guildID, tokenID string, expires time.Time) (string, error) {
	claims := Claims{
		GuildID: guildID,
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

// altchaKey derives the captcha's HMAC key from the JWT secret, so the
// deployment needs only one secret and the two never share a key.
func (t *Tokens) altchaKey() string {
	mac := hmac.New(sha256.New, t.secret)
	mac.Write([]byte("altcha"))
	return hex.EncodeToString(mac.Sum(nil))
}
