package verify

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"time"

	altcha "github.com/altcha-org/altcha-lib-go/v2"
)

// The proof of work is ALTCHA v2: the browser runs PBKDF2-SHA-256 at
// challengeCost iterations for counter after counter until the derived key
// matches the one the server picked. The server chooses the counter, so it
// signs the expected key up front and checks a solution without re-deriving.
//
// WebCrypto's PBKDF2 is native code, so a browser derives about as fast as a
// script would: some 13M iterations per second per core on a laptop. At 5,000
// iterations a counter of 3,000 to 6,000 is about 22M iterations, under two
// seconds on a laptop core and several on a phone, and the same CPU time for
// every link a script wants to solve. That is roughly four times what the old
// SHA-256 challenge cost a native solver.
const (
	challengeAlgorithm = "PBKDF2/SHA-256"
	challengeCost      = 5_000
	challengeCounterLo = 3_000
	challengeCounterHi = 6_000
	challengeTTL       = 10 * time.Minute
)

// challengeSecrets derives the two ALTCHA HMAC keys from the JWT secret, so
// the deployment needs only one secret and none of the three share a key.
func (t *Tokens) challengeSecrets() (sig, keySig string) {
	derive := func(label string) string {
		mac := hmac.New(sha256.New, t.secret)
		mac.Write([]byte(label))
		return hex.EncodeToString(mac.Sum(nil))
	}
	return derive("altcha"), derive("altcha-key")
}

// newChallenge returns a challenge bound to one link: the link's ID is signed
// into it, so a solution only ever unlocks the link it was fetched for.
func (t *Tokens) newChallenge(linkID string) (altcha.Challenge, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(challengeCounterHi-challengeCounterLo))
	if err != nil {
		return altcha.Challenge{}, err
	}
	counter := challengeCounterLo + int(n.Int64())
	expires := time.Now().Add(challengeTTL)
	sig, keySig := t.challengeSecrets()
	return altcha.CreateChallenge(altcha.CreateChallengeOptions{
		Algorithm:              challengeAlgorithm,
		Cost:                   challengeCost,
		Counter:                &counter,
		DeriveKey:              altcha.DeriveKeyPBKDF2(),
		ExpiresAt:              &expires,
		HMACSignatureSecret:    sig,
		HMACKeySignatureSecret: keySig,
		Data:                   map[string]any{"jti": linkID},
	})
}

// checkSolution reports whether payload, the widget's base64 JSON, solves a
// live challenge that was issued for linkID.
func (t *Tokens) checkSolution(payload, linkID string) bool {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var p altcha.Payload
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	if id, _ := p.Challenge.Parameters.Data["jti"].(string); id != linkID {
		return false
	}
	sig, keySig := t.challengeSecrets()
	res, err := altcha.VerifySolution(altcha.VerifySolutionOptions{
		Challenge:              p.Challenge,
		Solution:               p.Solution,
		DeriveKey:              altcha.DeriveKeyPBKDF2(),
		HMACSignatureSecret:    sig,
		HMACKeySignatureSecret: keySig,
	})
	if err != nil || !res.Verified || res.Expired {
		return false
	}
	// A solution is single-use: replaying it would let one proof of work
	// buy any number of requests until the challenge expires.
	return t.spend(p.Challenge.Signature)
}

// spend records a solved challenge and reports whether it was fresh.
func (t *Tokens) spend(sig string) bool {
	return t.spendUntil(sig, time.Now().Add(challengeTTL+time.Minute))
}

// spendUntil records a one-time key, remembered until the given time, and
// reports whether it was fresh.
func (t *Tokens) spendUntil(sig string, until time.Time) bool {
	if sig == "" {
		return false
	}
	t.spentMu.Lock()
	defer t.spentMu.Unlock()
	now := time.Now()
	for k, exp := range t.spent {
		if now.After(exp) {
			delete(t.spent, k)
		}
	}
	if _, dup := t.spent[sig]; dup {
		return false
	}
	t.spent[sig] = until
	return true
}
