package verify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altcha-org/altcha-lib-go"
	"github.com/golang-jwt/jwt/v5"
)

func TestTokenRoundTrip(t *testing.T) {
	tk := NewTokens("secret")
	raw, err := tk.Issue("u1", "g1", "", "jti1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	c, err := tk.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "u1" || c.GuildID != "g1" || c.ID != "jti1" {
		t.Fatalf("claims = %+v", c)
	}
}

func TestTokenRejectsForgedExpiredAndNone(t *testing.T) {
	tk := NewTokens("secret")

	forged, _ := NewTokens("other").Issue("u1", "g1", "", "j", time.Now().Add(time.Hour))
	if _, err := tk.Parse(forged); err == nil {
		t.Error("accepted a token signed with another secret")
	}

	expired, _ := tk.Issue("u1", "g1", "", "j", time.Now().Add(-time.Minute))
	if _, err := tk.Parse(expired); err == nil {
		t.Error("accepted an expired token")
	}

	none := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{GuildID: "g1", RegisteredClaims: jwt.RegisteredClaims{
		Issuer: issuer, Subject: "u1", ID: "j", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}})
	raw, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := tk.Parse(raw); err == nil {
		t.Error("accepted an unsigned token")
	}
}

// fakeLifter keeps one outstanding link, like the database does.
type fakeLifter struct {
	mu     sync.Mutex
	active string
	lifted int
}

func (f *fakeLifter) TokenActive(_ context.Context, c *Claims) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return c.ID == f.active, nil
}

func (f *fakeLifter) Lift(_ context.Context, c *Claims) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID != f.active {
		return ErrLinkUsed
	}
	f.active = ""
	f.lifted++
	return nil
}

func newTestServer(t *testing.T) (*httptest.Server, *Tokens, *fakeLifter) {
	t.Helper()
	tk := NewTokens("secret")
	fl := &fakeLifter{active: "jti1"}
	mux := http.NewServeMux()
	NewServer(tk, fl, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, tk, fl
}

// solvePayload does what the browser widget does: fetch a challenge, find
// the number, and encode the payload.
func solvePayload(t *testing.T, srv *httptest.Server, token string) string {
	t.Helper()
	res, err := http.Get(srv.URL + "/verify/challenge?t=" + url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("challenge status %d", res.StatusCode)
	}
	var ch altcha.Challenge
	if err := json.NewDecoder(res.Body).Decode(&ch); err != nil {
		t.Fatal(err)
	}
	sol, err := altcha.SolveChallenge(ch.Challenge, ch.Salt, altcha.Algorithm(ch.Algorithm), int(ch.MaxNumber), 0, nil)
	if err != nil || sol == nil {
		t.Fatalf("solve: %v", err)
	}
	p, _ := json.Marshal(altcha.Payload{
		Algorithm: ch.Algorithm, Challenge: ch.Challenge, Number: int64(sol.Number),
		Salt: ch.Salt, Signature: ch.Signature,
	})
	return base64.StdEncoding.EncodeToString(p)
}

func postSolve(t *testing.T, srv *httptest.Server, token, payload string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(solveRequest{Token: token, Payload: payload})
	res, err := http.Post(srv.URL+"/verify/solve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestSolveLiftsOnceThenLinkIsSpent(t *testing.T) {
	srv, tk, fl := newTestServer(t)
	token, _ := tk.Issue("u1", "g1", "", "jti1", time.Now().Add(time.Hour))

	payload := solvePayload(t, srv, token)
	if code, body := postSolve(t, srv, token, payload); code != http.StatusOK {
		t.Fatalf("solve = %d %v", code, body)
	}
	if fl.lifted != 1 {
		t.Fatalf("lifted = %d", fl.lifted)
	}

	// Replaying the same payload and link must not lift again.
	if code, body := postSolve(t, srv, token, payload); code != http.StatusGone || body["error"] != "invalid" {
		t.Fatalf("replay = %d %v", code, body)
	}

	// And the page now says the link is no longer valid.
	res, _ := http.Get(srv.URL + "/verify?t=" + url.QueryEscape(token))
	html, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(html), "altcha-widget challengeurl") || !strings.Contains(string(html), "δεν ισχύει πια") {
		t.Fatal("spent link still renders the challenge")
	}
}

func TestSolveRejectsBadCaptcha(t *testing.T) {
	srv, tk, fl := newTestServer(t)
	token, _ := tk.Issue("u1", "g1", "", "jti1", time.Now().Add(time.Hour))

	bogus := base64.StdEncoding.EncodeToString([]byte(`{"algorithm":"SHA-256","challenge":"x","number":1,"salt":"s","signature":"x"}`))
	if code, _ := postSolve(t, srv, token, bogus); code != http.StatusUnprocessableEntity {
		t.Fatalf("bogus captcha = %d", code)
	}
	if fl.lifted != 0 {
		t.Fatal("lifted on a bad captcha")
	}
}

func TestPageAndChallengeRequireLiveToken(t *testing.T) {
	srv, tk, _ := newTestServer(t)
	stale, _ := tk.Issue("u1", "g1", "", "old", time.Now().Add(time.Hour))

	res, _ := http.Get(srv.URL + "/verify/challenge?t=" + url.QueryEscape(stale))
	res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Fatalf("challenge for stale link = %d", res.StatusCode)
	}

	res, _ = http.Get(srv.URL + "/verify?t=garbage")
	res.Body.Close()
	if res.Header.Get("Referrer-Policy") != "no-referrer" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("missing security headers: %v", res.Header)
	}
}
