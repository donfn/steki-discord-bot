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

	altcha "github.com/altcha-org/altcha-lib-go/v2"
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

// fakeConsenter keeps one outstanding registration link.
type fakeConsenter struct {
	mu       sync.Mutex
	active   string
	taken    bool
	accepted int
}

func (f *fakeConsenter) Pending(_ context.Context, c *Claims) (*Consent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID != f.active {
		return nil, ErrLinkUsed
	}
	return &Consent{Username: "Μαρία <b>", AvatarURL: "https://cdn.discordapp.com/embed/avatars/1.png",
		Email: "th12345@edu.hmu.gr", University: "hmu"}, nil
}

func (f *fakeConsenter) Accept(_ context.Context, c *Claims) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID != f.active {
		return ErrLinkUsed
	}
	if f.taken {
		return ErrEmailTaken
	}
	f.active = ""
	f.accepted++
	return nil
}

type testEnv struct {
	srv *httptest.Server
	tk  *Tokens
	fl  *fakeLifter
	fc  *fakeConsenter
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{tk: NewTokens("secret"), fl: &fakeLifter{active: "jti1"}, fc: &fakeConsenter{active: "link1"}}
	mux := http.NewServeMux()
	NewServer(e.tk, e.fl, e.fc, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func newTestServer(t *testing.T) (*httptest.Server, *Tokens, *fakeLifter) {
	e := newEnv(t)
	return e.srv, e.tk, e.fl
}

// solveAt does what the browser widget does: fetch a challenge from path,
// derive keys counter by counter until one matches, and encode the payload.
func solveAt(t *testing.T, srv *httptest.Server, path, token string) string {
	t.Helper()
	res, err := http.Get(srv.URL + path + "?t=" + url.QueryEscape(token))
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
	if ch.Parameters.Algorithm != challengeAlgorithm || ch.Parameters.Cost != challengeCost {
		t.Fatalf("challenge parameters = %+v", ch.Parameters)
	}
	sol, err := altcha.SolveChallenge(altcha.SolveChallengeOptions{Challenge: ch, DeriveKey: altcha.DeriveKeyPBKDF2()})
	if err != nil || sol == nil {
		t.Fatalf("solve: %v", err)
	}
	p, _ := json.Marshal(altcha.Payload{Challenge: ch, Solution: *sol})
	return base64.StdEncoding.EncodeToString(p)
}

func solvePayload(t *testing.T, srv *httptest.Server, token string) string {
	return solveAt(t, srv, "/verify/challenge", token)
}

func postSolve(t *testing.T, srv *httptest.Server, token, payload string) (int, map[string]any) {
	return postTo(t, srv, "/verify/solve", token, payload)
}

func postTo(t *testing.T, srv *httptest.Server, path, token, payload string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(solveRequest{Token: token, Payload: payload})
	res, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(body))
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
	if strings.Contains(string(html), "<altcha-widget") || !strings.Contains(string(html), "δεν ισχύει πια") {
		t.Fatal("spent link still renders the challenge")
	}
}

func TestSolveRejectsBadCaptcha(t *testing.T) {
	srv, tk, fl := newTestServer(t)
	token, _ := tk.Issue("u1", "g1", "", "jti1", time.Now().Add(time.Hour))

	bogus := base64.StdEncoding.EncodeToString([]byte(
		`{"challenge":{"parameters":{"algorithm":"PBKDF2/SHA-256","nonce":"00","salt":"00","cost":1,"keyLength":32,"keyPrefix":"00","data":{"jti":"jti1"}},"signature":"x"},"solution":{"counter":1,"derivedKey":"00"}}`))
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

// A solution fetched for one link must not unlock another.
func TestSolutionIsBoundToItsLink(t *testing.T) {
	srv, tk, fl := newTestServer(t)
	mine, _ := tk.Issue("u1", "g1", "", "jti1", time.Now().Add(time.Hour))
	other, _ := tk.Issue("u2", "g1", "", "jti2", time.Now().Add(time.Hour))
	fl.active = "jti1"

	payload := solvePayload(t, srv, mine)
	fl.active = "jti2"
	if code, _ := postSolve(t, srv, other, payload); code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-link solve = %d, want 422", code)
	}
	if fl.lifted != 0 {
		t.Fatal("lifted with another link's solution")
	}
}

func TestPageCSPNonceMatchesScripts(t *testing.T) {
	srv, tk, _ := newTestServer(t)
	token, _ := tk.Issue("u1", "g1", "", "jti1", time.Now().Add(time.Hour))
	res, err := http.Get(srv.URL + "/verify?t=" + url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	csp := res.Header.Get("Content-Security-Policy")
	_, after, ok := strings.Cut(csp, "'nonce-")
	if !ok {
		t.Fatalf("no nonce in CSP %q", csp)
	}
	nonce, _, _ := strings.Cut(after, "'")
	if n := strings.Count(string(body), `nonce="`+nonce+`"`); n != 2 {
		t.Fatalf("scripts carrying the CSP nonce = %d, want 2", n)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") || res.Header.Get("Strict-Transport-Security") == "" {
		t.Fatalf("missing hardening headers: %v", res.Header)
	}
}

func TestLinkPageShowsTheConnection(t *testing.T) {
	e := newEnv(t)
	token, _ := e.tk.IssueLink("u1", "g1", "link1", time.Now().Add(time.Hour))
	res, err := http.Get(e.srv.URL + "/link?t=" + url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	html := string(body)
	for _, want := range []string{
		"th12345@edu.hmu.gr", "/verify/uni/hmu.png", "cdn.discordapp.com/embed/avatars/1.png",
		"Μαρία &lt;b&gt;", `display="invisible"`, "/link/challenge?t=",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("link page lacks %q", want)
		}
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "img-src 'self' data: https://cdn.discordapp.com") {
		t.Errorf("CSP does not allow Discord avatars: %s", res.Header.Get("Content-Security-Policy"))
	}
}

func TestLinkAcceptOnceThenSpent(t *testing.T) {
	e := newEnv(t)
	token, _ := e.tk.IssueLink("u1", "g1", "link1", time.Now().Add(time.Hour))
	payload := solveAt(t, e.srv, "/link/challenge", token)

	if code, body := postTo(t, e.srv, "/link/accept", token, payload); code != http.StatusOK {
		t.Fatalf("accept = %d %v", code, body)
	}
	if code, body := postTo(t, e.srv, "/link/accept", token, payload); code != http.StatusGone || body["error"] != "invalid" {
		t.Fatalf("replay = %d %v", code, body)
	}
	if e.fc.accepted != 1 {
		t.Fatalf("accepted = %d", e.fc.accepted)
	}
}

func TestLinkAcceptReportsTakenAddress(t *testing.T) {
	e := newEnv(t)
	e.fc.taken = true
	token, _ := e.tk.IssueLink("u1", "g1", "link1", time.Now().Add(time.Hour))
	payload := solveAt(t, e.srv, "/link/challenge", token)
	if code, body := postTo(t, e.srv, "/link/accept", token, payload); code != http.StatusConflict || body["error"] != "taken" {
		t.Fatalf("taken = %d %v", code, body)
	}
}

// A guard token must not open the link page and a link token must not lift a
// timeout, even though both are signed with the same secret.
func TestTokensDoNotCrossPages(t *testing.T) {
	e := newEnv(t)
	e.fc.active = "jti1"
	guard, _ := e.tk.Issue("u1", "g1", ReasonMedia, "jti1", time.Now().Add(time.Hour))
	link, _ := e.tk.IssueLink("u1", "g1", "jti1", time.Now().Add(time.Hour))

	res, _ := http.Get(e.srv.URL + "/link/challenge?t=" + url.QueryEscape(guard))
	res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Errorf("guard token on link challenge = %d", res.StatusCode)
	}
	res, _ = http.Get(e.srv.URL + "/verify/challenge?t=" + url.QueryEscape(link))
	res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Errorf("link token on guard challenge = %d", res.StatusCode)
	}

	payload := solveAt(t, e.srv, "/verify/challenge", guard)
	if code, _ := postTo(t, e.srv, "/link/accept", guard, payload); code != http.StatusGone {
		t.Errorf("guard token accepted a link = %d", code)
	}
	if code, _ := postTo(t, e.srv, "/verify/solve", link, payload); code != http.StatusGone {
		t.Errorf("link token lifted a timeout = %d", code)
	}
	if e.fc.accepted != 0 || e.fl.lifted != 0 {
		t.Fatal("a token worked on the wrong page")
	}
}
