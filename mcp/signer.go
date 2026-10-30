package mcp

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Every token the server mints is a JWT signed with the same Ed25519 key and
// told apart by its "typ" header, so a token made for one step (a client ID, a
// login state, an approval, an authorization code) can never be presented as
// another, and none of them as an access token.
const (
	typClient   = "steki-mcp-client+jwt"
	typState    = "steki-mcp-state+jwt"
	typApproval = "steki-mcp-approval+jwt"
	typCode     = "steki-mcp-code+jwt"
	// typAccess is the RFC 9068 type of a JWT access token.
	typAccess = "at+jwt"
)

// signer signs and verifies the server's tokens. The key is derived from the
// bot's JWT_SECRET, so it is stable across restarts and deploys with no key
// file to keep, and rotating the secret revokes every token at once.
type signer struct {
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey
	kid    string
	x      string
	issuer string
}

func newSigner(secret, issuer string) *signer {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("steki-mcp/ed25519-signing-key/v1"))
	priv := ed25519.NewKeyFromSeed(mac.Sum(nil))
	pub := priv.Public().(ed25519.PublicKey)

	x := base64.RawURLEncoding.EncodeToString(pub)
	// The key ID is the RFC 7638 thumbprint of the public key.
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + x + `"}`))
	return &signer{
		priv:   priv,
		pub:    pub,
		kid:    base64.RawURLEncoding.EncodeToString(sum[:]),
		x:      x,
		issuer: issuer,
	}
}

// sign returns claims as a compact JWT of the given type.
func (s *signer) sign(typ string, claims jwt.Claims) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	t.Header["typ"] = typ
	t.Header["kid"] = s.kid
	return t.SignedString(s.priv)
}

// parse checks raw's signature, type, issuer, audience and expiry and fills in
// claims.
func (s *signer) parse(typ, audience, raw string, claims jwt.Claims) error {
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if got, _ := t.Header["typ"].(string); got != typ {
			return nil, errors.New("mcp: wrong token type")
		}
		return s.pub, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5*time.Second),
	)
	return err
}

// jwks is the public key as a JSON Web Key Set, for clients that want to
// verify access tokens themselves.
func (s *signer) jwks() map[string]any {
	return map[string]any{"keys": []map[string]any{{
		"kty": "OKP", "crv": "Ed25519", "x": s.x,
		"kid": s.kid, "use": "sig", "alg": "EdDSA",
	}}}
}
