package verify

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDiscord answers the OAuth2 calls an Activity login makes.
type fakeDiscord struct {
	mu      sync.Mutex
	revoked []string
}

func (d *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/oauth2/token":
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "shh" || r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("redirect_uri") != "" {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		users := map[string]string{"good": "at-42", "other": "at-43"}
		at, ok := users[r.Form.Get("code")]
		if !ok {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": at, "token_type": "Bearer"})
	case "/oauth2/token/revoke":
		_ = r.ParseForm()
		d.mu.Lock()
		d.revoked = append(d.revoked, r.Form.Get("token"))
		d.mu.Unlock()
	case "/users/@me":
		id, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer at-")
		if !ok {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "username": "x"})
	default:
		http.NotFound(w, r)
	}
}

// fakeAccess lets the tests say who may read what.
type fakeAccess struct {
	mu       sync.Mutex
	members  map[string]bool            // verified members
	channels map[string]map[string]bool // user -> channels they can see
}

func (a *fakeAccess) GuildID() string { return "g1" }

func (a *fakeAccess) CanRead(_ context.Context, userID, channelID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.members[userID] {
		return ErrForbidden
	}
	if !a.channels[userID][channelID] {
		return ErrNoMaterial
	}
	return nil
}

func (a *fakeAccess) Courses(_ context.Context, userID string) ([]ActivityCourse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.members[userID] {
		return nil, ErrForbidden
	}
	var out []ActivityCourse
	for _, id := range []string{"100", "200"} {
		if a.channels[userID][id] {
			out = append(out, ActivityCourse{ID: id, Name: "c" + id, Category: "Α' εξάμηνο", Files: 2})
		}
	}
	return out, nil
}

func (a *fakeAccess) revoke(userID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.members[userID] = false
}

type activityEnv struct {
	srv     *httptest.Server
	tk      *Tokens
	discord *fakeDiscord
	access  *fakeAccess
	rep     *fakeReporter
	priv    ed25519.PrivateKey
}

// newActivityEnv serves the Activity behind Harden, as main does. With
// signed, it checks Discord's proxy signatures.
func newActivityEnv(t *testing.T, signed bool) *activityEnv {
	t.Helper()
	e := &activityEnv{
		tk:      NewTokens("secret"),
		discord: &fakeDiscord{},
		access: &fakeAccess{
			members:  map[string]bool{"42": true, "43": true},
			channels: map[string]map[string]bool{"42": {"100": true}, "43": {"200": true}},
		},
	}
	api := httptest.NewServer(e.discord)
	t.Cleanup(api.Close)

	fm := newFakeMaterials()
	e.rep = &fakeReporter{fakeMaterials: fm, res: &ReportResult{Left: 4}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	v := NewServer(e.tk, &fakeLifter{}, &fakeConsenter{}, e.rep, logger)
	opts := ActivityOptions{ClientID: "123", ClientSecret: "shh", Access: e.access, DiscordAPI: api.URL, HTTPClient: api.Client()}
	if signed {
		pub, priv, _ := ed25519.GenerateKey(nil)
		e.priv = priv
		opts.PublicKey = hex.EncodeToString(pub)
	}
	if err := v.EnableActivity(opts); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	v.Register(mux)
	e.srv = httptest.NewServer(Harden(mux, logger))
	t.Cleanup(e.srv.Close)
	return e
}

// proxyHeaders signs a request the way Discord's proxy does.
func (e *activityEnv) proxyHeaders(userID string, created, expires int64) map[string]string {
	payload := fmt.Sprintf(`{"created_at":%d,"expires_at":%d,"user_id":%q}`, created, expires, userID)
	sig := ed25519.Sign(e.priv, []byte(payload))
	return map[string]string{
		proxyPayloadHeader:   base64.StdEncoding.EncodeToString([]byte(payload)),
		proxySignatureHeader: hex.EncodeToString(sig),
		proxyTimestampHeader: fmt.Sprint(created),
	}
}

func (e *activityEnv) do(t *testing.T, method, path, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

// login does what the page does after the SDK hands it a code.
func (e *activityEnv) login(t *testing.T, code string) string {
	t.Helper()
	res, body := e.do(t, http.MethodPost, "/activity/token", `{"code":"`+code+`"}`, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	var out struct {
		Token   string `json:"token"`
		Expires int64  `json:"expires"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Token == "" {
		t.Fatalf("login body %q", body)
	}
	return out.Token
}

func TestActivityTokenExchange(t *testing.T) {
	e := newActivityEnv(t, false)
	tok := e.login(t, "good")
	c, err := e.tk.Parse(tok)
	if err != nil || c.Subject != "42" || c.Reason != ReasonActivity || c.GuildID != "g1" {
		t.Fatalf("claims %+v, %v", c, err)
	}
	if left := time.Until(c.ExpiresAt.Time); left > activityTokenTTL || left < activityTokenTTL-time.Minute {
		t.Errorf("token lives %v", left)
	}
	e.discord.mu.Lock()
	revoked := e.discord.revoked
	e.discord.mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "at-42" {
		t.Errorf("discord token not revoked: %v", revoked)
	}

	for _, body := range []string{`{"code":"forged"}`, `{"code":""}`, `{}`, `not json`} {
		res, _ := e.do(t, http.MethodPost, "/activity/token", body, nil)
		if res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, res.StatusCode)
		}
	}
}

func TestActivityBearerChecks(t *testing.T) {
	e := newActivityEnv(t, false)
	good := e.login(t, "good")
	material, _ := e.tk.IssueMaterials("42", "g1", "100", "m1", time.Now().Add(time.Minute))
	guard, _ := e.tk.Issue("42", "g1", ReasonMedia, "j1", time.Now().Add(time.Minute))
	expired, _ := e.tk.IssueActivity("42", "g1", "a1", time.Now().Add(-time.Second))
	forged, _ := NewTokens("other").IssueActivity("42", "g1", "a2", time.Now().Add(time.Minute))

	cases := map[string]map[string]string{
		"no header":      nil,
		"material token": bearer(material),
		"guard token":    bearer(guard),
		"expired":        bearer(expired),
		"forged":         bearer(forged),
		"not bearer":     {"Authorization": good},
	}
	for name, hdr := range cases {
		for _, path := range []string{"/activity/data?channel=100", "/activity/f/11?channel=100", "/activity/courses"} {
			if res, _ := e.do(t, http.MethodGet, path, "", hdr); res.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s: status %d, want 401", name, path, res.StatusCode)
			}
		}
	}

	// The token is no good anywhere else either.
	v := &Server{tokens: e.tk}
	if _, ok := v.guardClaims(good); ok {
		t.Error("the media guard accepts an Activity token")
	}
	if _, ok := v.materialClaims(good); ok {
		t.Error("the material link accepts an Activity token")
	}
}

func TestActivityAccessIsCheckedEveryRequest(t *testing.T) {
	e := newActivityEnv(t, false)
	tok := e.login(t, "good")

	if res, body := e.do(t, http.MethodGet, "/activity/data?channel=200", "", bearer(tok)); res.StatusCode != http.StatusForbidden ||
		!strings.Contains(body, `"channel"`) {
		t.Errorf("unseen channel: %d %s", res.StatusCode, body)
	}
	if res, _ := e.do(t, http.MethodGet, "/activity/f/21?channel=200", "", bearer(tok)); res.StatusCode != http.StatusForbidden {
		t.Errorf("file of an unseen channel: %d", res.StatusCode)
	}
	// A file of another channel, asked for under one the member can see.
	if res, _ := e.do(t, http.MethodGet, "/activity/f/21?channel=100", "", bearer(tok)); res.StatusCode != http.StatusGone {
		t.Errorf("file of another channel: %d", res.StatusCode)
	}
	if res, _ := e.do(t, http.MethodGet, "/activity/data?channel=abc", "", bearer(tok)); res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad channel: %d", res.StatusCode)
	}

	// The role goes: the still-valid token stops working at once.
	e.access.revoke("42")
	for _, path := range []string{"/activity/data?channel=100", "/activity/f/11?channel=100", "/activity/courses"} {
		res, body := e.do(t, http.MethodGet, path, "", bearer(tok))
		if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "forbidden") {
			t.Errorf("revoked %s: %d %s", path, res.StatusCode, body)
		}
	}
}

func TestActivityDataFilesAndReports(t *testing.T) {
	e := newActivityEnv(t, false)
	tok := e.login(t, "good")

	res, body := e.do(t, http.MethodGet, "/activity/data?channel=100", "", bearer(tok))
	if res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("data: %d %v", res.StatusCode, res.Header)
	}
	var data struct {
		Channel string         `json:"channel"`
		Course  string         `json:"course"`
		Files   []MaterialFile `json:"files"`
		Total   int64          `json:"total"`
	}
	if err := json.Unmarshal([]byte(body), &data); err != nil || data.Channel != "μαθηματικά-ι" ||
		data.Course != "Μαθηματικά ι" || len(data.Files) != 2 || data.Total != 11 {
		t.Fatalf("data %s", body)
	}

	// The Activity is reusable: every file downloads as often as asked.
	for range 3 {
		res, body = e.do(t, http.MethodGet, "/activity/f/11?channel=100", "", bearer(tok))
		if res.StatusCode != http.StatusOK || body != "%PDF-1.7" || res.Header.Get("Content-Type") != "application/pdf" ||
			res.Header.Get("Content-Length") != "8" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("file: %d %q %v", res.StatusCode, body, res.Header)
		}
	}

	res, body = e.do(t, http.MethodGet, "/activity/courses", "", bearer(tok))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"id":"100"`) || strings.Contains(body, `"id":"200"`) {
		t.Errorf("courses: %d %s", res.StatusCode, body)
	}

	report := `{"channel":"100","file":"11","reason":"δεν είναι υλικό μαθήματος"}`
	res, body = e.do(t, http.MethodPost, "/activity/report", report, bearer(tok))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"left":4`) {
		t.Fatalf("report: %d %s", res.StatusCode, body)
	}
	if len(e.rep.got) != 1 || e.rep.got[0] != "42/100/11/δεν είναι υλικό μαθήματος" {
		t.Errorf("reporter got %v", e.rep.got)
	}
	if res, _ := e.do(t, http.MethodPost, "/activity/report", strings.Replace(report, `"100"`, `"200"`, 1), bearer(tok)); res.StatusCode != http.StatusForbidden {
		t.Errorf("report on an unseen channel: %d", res.StatusCode)
	}
	if res, _ := e.do(t, http.MethodPost, "/activity/report", report, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("report without a token: %d", res.StatusCode)
	}
}

func TestActivityPageCanBeFramedByDiscordOnly(t *testing.T) {
	e := newActivityEnv(t, false)
	for _, path := range []string{"/activity", "/activity/"} {
		res, body := e.do(t, http.MethodGet, path, "", nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, res.StatusCode)
		}
		if xfo := res.Header.Get("X-Frame-Options"); xfo != "" {
			t.Errorf("%s: X-Frame-Options %q", path, xfo)
		}
		if res.Header.Get("Cross-Origin-Opener-Policy") != "" {
			t.Errorf("%s: COOP set", path)
		}
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors https://discord.com https://*.discord.com https://*.discordsays.com") ||
			strings.Contains(csp, "frame-ancestors 'none'") ||
			strings.Contains(csp, "jsdelivr") || strings.Contains(csp, "googleapis") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", path, res.Header)
		}
		nonce := strings.TrimSuffix(strings.SplitN(strings.SplitN(csp, "'nonce-", 2)[1], "'", 2)[0], "'")
		if !strings.Contains(body, `nonce="`+nonce+`"`) {
			t.Errorf("%s: scripts do not carry the CSP nonce", path)
		}
		for _, want := range []string{`clientId: "123"`, "static/sdk-2.5.0.mjs", "static/pdfjs-4.10.38/", "Χώρος μελέτης"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: page lacks %q", path, want)
			}
		}
		for _, bad := range []string{"cdn.jsdelivr.net/npm/pdfjs-dist@4.10.38/legacy", "fonts.googleapis.com/css2", `id="data"`} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: page loads %q", path, bad)
			}
		}
	}

	// Everything else still refuses to be framed.
	if res, _ := e.do(t, http.MethodGet, "/verify", "", nil); res.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("other pages lost X-Frame-Options")
	}
}

func TestActivityStaticFiles(t *testing.T) {
	e := newActivityEnv(t, false)
	cases := map[string]string{
		"/activity/static/sdk-2.5.0.mjs":                                 "text/javascript; charset=utf-8",
		"/activity/static/pdfjs-4.10.38/legacy/build/pdf.min.mjs":        "text/javascript; charset=utf-8",
		"/activity/static/pdfjs-4.10.38/legacy/build/pdf.worker.min.mjs": "text/javascript; charset=utf-8",
		"/activity/static/fonts-5.3.0/fonts.css":                         "text/css; charset=utf-8",
		"/activity/static/fonts-5.3.0/fira-sans-greek-400-normal.woff2":  "font/woff2",
		"/activity/static/logo.png":                                      "image/png",
	}
	for path, ctype := range cases {
		res, body := e.do(t, http.MethodGet, path, "", nil)
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != ctype || len(body) == 0 ||
			!strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %q %q", path, res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Cache-Control"))
		}
	}
	for _, path := range []string{"/activity/static/nope.js", "/activity/static/../server.go", "/activity/static/pdfjs-4.10.38/"} {
		if res, _ := e.do(t, http.MethodGet, path, "", nil); res.StatusCode == http.StatusOK {
			t.Errorf("%s served", path)
		}
	}
}

func TestActivitySkipsCrossSiteChecks(t *testing.T) {
	e := newActivityEnv(t, false)
	body := `{"code":"good"}`
	// Whatever Discord's proxy sends as the origin, the login goes through:
	// the Activity's writes carry no cookie.
	for _, hdr := range []map[string]string{
		{"Origin": "https://123.discordsays.com", "Sec-Fetch-Site": "same-origin"},
		{"Origin": "https://discord.com", "Sec-Fetch-Site": "cross-site"},
	} {
		if res, _ := e.do(t, http.MethodPost, "/activity/token", body, hdr); res.StatusCode != http.StatusOK {
			t.Errorf("%v: %d", hdr, res.StatusCode)
		}
	}
	// A cross-site report still needs a valid token.
	res, _ := e.do(t, http.MethodPost, "/activity/report", `{"channel":"100","file":"11","reason":"δεν είναι υλικό μαθήματος"}`,
		map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-site report without a token: %d", res.StatusCode)
	}
	// Elsewhere the cookie routes keep their checks.
	if res, _ := e.do(t, http.MethodPost, "/s/report", `{}`, map[string]string{"Origin": "https://123.discordsays.com"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("discordsays origin on /s/report: %d", res.StatusCode)
	}
}

func TestActivityProxyAuthentication(t *testing.T) {
	e := newActivityEnv(t, true)
	now := time.Now().Unix()

	// Unsigned requests are refused, the page and static files included.
	for _, path := range []string{"/activity", "/activity/static/sdk-2.5.0.mjs", "/activity/courses"} {
		if res, _ := e.do(t, http.MethodGet, path, "", nil); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("unsigned %s: %d", path, res.StatusCode)
		}
	}

	// The proxy sends POSTs unsigned: the login needs a valid code, and the
	// report a valid token, but neither a signature. A bad one still fails.
	if res, _ := e.do(t, http.MethodPost, "/activity/token", `{"code":"good"}`, nil); res.StatusCode != http.StatusOK {
		t.Errorf("unsigned login: %d", res.StatusCode)
	}
	if res, _ := e.do(t, http.MethodPost, "/activity/token", `{"code":"forged"}`, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned login with a forged code: %d", res.StatusCode)
	}
	if res, _ := e.do(t, http.MethodPost, "/activity/report", `{"channel":"100","file":"11","reason":"δεν είναι υλικό μαθήματος"}`, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned report without a token: %d", res.StatusCode)
	}
	expiredSig := e.proxyHeaders("42", now-120, now-60)
	if res, _ := e.do(t, http.MethodPost, "/activity/token", `{"code":"good"}`, expiredSig); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with an expired signature: %d", res.StatusCode)
	}

	signed := e.proxyHeaders("42", now, now+60)
	if res, _ := e.do(t, http.MethodGet, "/activity", "", signed); res.StatusCode != http.StatusOK {
		t.Fatalf("signed page: %d", res.StatusCode)
	}
	res, body := e.do(t, http.MethodPost, "/activity/token", `{"code":"good"}`, signed)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("signed login: %d %s", res.StatusCode, body)
	}
	var out struct{ Token string }
	_ = json.Unmarshal([]byte(body), &out)

	with := func(h map[string]string) map[string]string {
		m := map[string]string{"Authorization": "Bearer " + out.Token}
		for k, v := range h {
			m[k] = v
		}
		return m
	}
	if res, _ := e.do(t, http.MethodGet, "/activity/data?channel=100", "", with(signed)); res.StatusCode != http.StatusOK {
		t.Errorf("signed data: %d", res.StatusCode)
	}
	// The signature also checks out base64 encoded, as one of the docs' examples has it.
	b64 := e.proxyHeaders("42", now, now+60)
	sig, _ := hex.DecodeString(b64[proxySignatureHeader])
	b64[proxySignatureHeader] = base64.StdEncoding.EncodeToString(sig)
	if res, _ := e.do(t, http.MethodGet, "/activity/data?channel=100", "", with(b64)); res.StatusCode != http.StatusOK {
		t.Errorf("base64 signature: %d", res.StatusCode)
	}

	bad := map[string]map[string]string{
		"expired":        e.proxyHeaders("42", now-120, now-60),
		"another member": e.proxyHeaders("43", now, now+60),
		"timestamp mismatch": func() map[string]string {
			h := e.proxyHeaders("42", now, now+60)
			h[proxyTimestampHeader] = "1"
			return h
		}(),
		"tampered payload": func() map[string]string {
			h := e.proxyHeaders("42", now, now+60)
			h[proxyPayloadHeader] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"created_at":%d,"expires_at":%d,"user_id":"42"}`, now, now+6000)))
			return h
		}(),
		"other key": func() map[string]string {
			_, other, _ := ed25519.GenerateKey(nil)
			saved := e.priv
			e.priv = other
			defer func() { e.priv = saved }()
			return e.proxyHeaders("42", now, now+60)
		}(),
	}
	for name, h := range bad {
		if res, _ := e.do(t, http.MethodGet, "/activity/data?channel=100", "", with(h)); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, res.StatusCode)
		}
	}
}

func TestEnableActivityRejectsBadKey(t *testing.T) {
	v := NewServer(NewTokens("s"), &fakeLifter{}, &fakeConsenter{}, newFakeMaterials(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, key := range []string{"zz", "abcd", strings.Repeat("ab", 33)} {
		if err := v.EnableActivity(ActivityOptions{PublicKey: key}); err == nil {
			t.Errorf("key %q accepted", key)
		}
	}
}

func TestActivityTokenHeaderFallback(t *testing.T) {
	e := newActivityEnv(t, false)
	tok := e.login(t, "good")
	hdr := map[string]string{"X-Activity-Token": tok, "Authorization": "Basic proxy-owned"}
	if res, _ := e.do(t, http.MethodGet, "/activity/courses", "", hdr); res.StatusCode != http.StatusOK {
		t.Errorf("token in X-Activity-Token: %d", res.StatusCode)
	}
	if res, _ := e.do(t, http.MethodGet, "/activity/courses", "", map[string]string{"X-Activity-Token": "nope"}); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token in X-Activity-Token: %d", res.StatusCode)
	}
}
