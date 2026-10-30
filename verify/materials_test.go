package verify

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fakeMaterials struct {
	channels map[string]*MaterialChannel
	bodies   map[string]string // "channel/file" -> content
}

func newFakeMaterials() *fakeMaterials {
	return &fakeMaterials{
		channels: map[string]*MaterialChannel{
			"100": {Name: "μαθηματικά-ι", Files: []MaterialFile{
				{ID: "11", Name: "θέματα.pdf", Type: "application/pdf", Size: 8, PostedAt: time.Unix(1700000000, 0)},
				{ID: "12", Name: "<script>.jpg", Type: "image/jpeg", Size: 3, Caption: `</script><b>x</b>`},
			}},
			"200": {Name: "φυσική", Files: []MaterialFile{{ID: "21", Name: "other.pdf", Type: "application/pdf", Size: 5}}},
		},
		bodies: map[string]string{"100/11": "%PDF-1.7", "100/12": "\xff\xd8\xff", "200/21": "%PDF-"},
	}
}

func (f *fakeMaterials) Channel(_ context.Context, channelID string) (*MaterialChannel, error) {
	if ch, ok := f.channels[channelID]; ok {
		return ch, nil
	}
	return nil, ErrNoMaterial
}

func (f *fakeMaterials) Open(_ context.Context, channelID, fileID string) (*MaterialBody, error) {
	body, ok := f.bodies[channelID+"/"+fileID]
	if !ok {
		return nil, ErrNoMaterial
	}
	typ := "application/pdf"
	for _, mf := range f.channels[channelID].Files {
		if mf.ID == fileID {
			typ = mf.Type
		}
	}
	return &MaterialBody{Body: io.NopCloser(strings.NewReader(body)), Size: int64(len(body)), Type: typ}, nil
}

func (e *testEnv) materialToken(t *testing.T, channelID string, ttl time.Duration) string {
	t.Helper()
	tok, err := e.tk.IssueMaterials("u1", "g1", channelID, "m1", time.Now().Add(ttl))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// browser is a client that keeps cookies and follows redirects, like one.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func fetch(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// open follows a link as a browser would and returns its client, holding the
// session, along with the page it landed on.
func (e *testEnv) open(t *testing.T, channelID string) (*http.Client, *http.Response, string) {
	t.Helper()
	c := browser(t)
	res, body := fetch(t, c, e.srv.URL+"/materials?t="+e.materialToken(t, channelID, time.Minute))
	return c, res, body
}

func TestLinkOpensASessionAndRedirects(t *testing.T) {
	e := newEnv(t)
	tok := e.materialToken(t, "100", time.Minute)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, _ := fetch(t, c, e.srv.URL+"/materials?t="+tok)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/s" {
		t.Fatalf("status %d, location %q; want 303 to /s", res.StatusCode, res.Header.Get("Location"))
	}
	if strings.Contains(res.Header.Get("Location"), tok) {
		t.Error("the link survived into the redirect")
	}
	var cookie *http.Cookie
	for _, ck := range res.Cookies() {
		if ck.Name == sessionCookie {
			cookie = ck
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Value == "" {
		t.Fatalf("session cookie = %+v", cookie)
	}
}

func TestMaterialsPageLists(t *testing.T) {
	e := newEnv(t)
	_, res, body := e.open(t, "100")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	for _, want := range []string{"μαθηματικά-ι", "θέματα.pdf", `"id":"11"`, "pdfjs-dist@4.10.38"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The page shares its script with the Activity, which sends tokens; what
	// must not be in it is one.
	if strings.Contains(body, "eyJ") || strings.Contains(body, `"token":`) {
		t.Error("the page still carries a token")
	}
	// Names and captions are data inside a script block; none may close it.
	if strings.Contains(body, "</script><b>") || strings.Contains(body, "<script>.jpg") {
		t.Error("file metadata was not escaped")
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "worker-src blob:") ||
		!strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("unexpected CSP %q", csp)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("page must not be cached")
	}
}

var styleBlock = regexp.MustCompile(`(?s)<style.*?</style>`)

// expiredPage checks a response is the generic expired page: gone, no file
// data, and nothing that says what the page was for.
func expiredPage(t *testing.T, name string, res *http.Response, body string) {
	t.Helper()
	if res.StatusCode != http.StatusGone {
		t.Errorf("%s: status %d, want 410", name, res.StatusCode)
	}
	if !strings.Contains(body, "Ο σύνδεσμος έληξε") || strings.Contains(body, `"files"`) {
		t.Errorf("%s: not the expired page", name)
	}
	// The stylesheet is shared with the page proper and names its classes;
	// what a visitor reads, and the markup around it, must not.
	visible := styleBlock.ReplaceAllString(body, "")
	for _, hint := range []string{"Υλικό", "υλικ", "material", "μαθήματ", "pdf", "Discord"} {
		if strings.Contains(visible, hint) {
			t.Errorf("%s: the expired page hints at %q", name, hint)
		}
	}
}

func TestRefreshShowsGenericExpiredPage(t *testing.T) {
	e := newEnv(t)
	c, res, _ := e.open(t, "100")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first view: status %d", res.StatusCode)
	}
	// A refresh asks for the page again.
	res, body := fetch(t, c, e.srv.URL+"/s")
	expiredPage(t, "refresh", res, body)
	// The refresh also revoked the session: nothing downloads any more.
	if res, _ := fetch(t, c, e.srv.URL+"/s/f/11"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("download after refresh: status %d, want 401", res.StatusCode)
	}
	// And so does a third look.
	res, body = fetch(t, c, e.srv.URL+"/s")
	expiredPage(t, "third view", res, body)
}

func TestMaterialsPageRefusesOtherTokens(t *testing.T) {
	e := newEnv(t)
	guard, _ := e.tk.Issue("u1", "g1", ReasonMedia, "jti1", time.Now().Add(time.Minute))
	link, _ := e.tk.IssueLink("u1", "g1", "link1", time.Now().Add(time.Minute))
	for name, tok := range map[string]string{
		"guard":    guard,
		"link":     link,
		"expired":  e.materialToken(t, "100", -time.Minute),
		"garbage":  "abc",
		"no token": "",
	} {
		res, body := fetch(t, browser(t), e.srv.URL+"/materials?t="+tok)
		expiredPage(t, name+" token", res, body)
	}
	// A channel without a page is only found out when the page is asked for.
	c, res, body := e.open(t, "999")
	expiredPage(t, "no page", res, body)
	if res, _ := fetch(t, c, e.srv.URL+"/s/f/11"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("download without a page: status %d, want 401", res.StatusCode)
	}
}

func TestPageWithoutASessionIsExpired(t *testing.T) {
	e := newEnv(t)
	res, body := fetch(t, browser(t), e.srv.URL+"/s")
	expiredPage(t, "no cookie", res, body)
}

func TestGuardRefusesMaterialToken(t *testing.T) {
	e := newEnv(t)
	tok := e.materialToken(t, "100", time.Minute)
	if _, ok := (&Server{tokens: e.tk}).guardClaims(tok); ok {
		t.Error("media guard accepted a material token")
	}
	if _, ok := (&Server{tokens: e.tk}).linkClaims(tok); ok {
		t.Error("link page accepted a material token")
	}
}

func TestMaterialFile(t *testing.T) {
	e := newEnv(t)
	c, _, _ := e.open(t, "100")

	res, body := fetch(t, c, e.srv.URL+"/s/f/11")
	if res.StatusCode != http.StatusOK || body != "%PDF-1.7" || res.Header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("pdf: status %d type %q body %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	if res, _ := fetch(t, c, e.srv.URL+"/s/f/..%2f11"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id: status %d, want 400", res.StatusCode)
	}
	// Another channel's file is not on this session's page.
	if res, _ := fetch(t, c, e.srv.URL+"/s/f/21"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-channel file: status %d, want 401", res.StatusCode)
	}
	// Neither the old bearer header nor a cookie-less request gets in.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/s/f/11", nil)
	req.Header.Set("Authorization", "Bearer "+e.materialToken(t, "100", time.Minute))
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bearer token: %v %v, want 401", res, err)
	}
	// A session whose page was never shown downloads nothing.
	sess, _ := fetch(t, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		e.srv.URL+"/materials?t="+e.materialToken(t, "100", time.Minute))
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/s/f/11", nil)
	req.AddCookie(sess.Cookies()[0])
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("download before the page: %v %v, want 401", res, err)
	}
}

func TestSessionEndsWhenEveryFileIsDownloaded(t *testing.T) {
	e := newEnv(t)
	c, _, _ := e.open(t, "100")

	if res, _ := fetch(t, c, e.srv.URL+"/s/f/11"); res.StatusCode != http.StatusOK {
		t.Fatalf("first file: status %d", res.StatusCode)
	}
	// One file is still to come: the session lives, and a file may be fetched again.
	if res, _ := fetch(t, c, e.srv.URL+"/s/f/11"); res.StatusCode != http.StatusOK {
		t.Fatalf("repeat download: status %d, want 200", res.StatusCode)
	}
	if res, _ := fetch(t, c, e.srv.URL+"/s/f/12"); res.StatusCode != http.StatusOK {
		t.Fatalf("last file: status %d", res.StatusCode)
	}
	// Every file is down: the session is gone.
	for _, id := range []string{"11", "12"} {
		if res, _ := fetch(t, c, e.srv.URL+"/s/f/"+id); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("file %s after completion: status %d, want 401", id, res.StatusCode)
		}
	}
}

func TestSessionRunsOut(t *testing.T) {
	s := newSessions()
	now := time.Now()
	id, ok := s.open("100", "u1", now)
	if !ok {
		t.Fatal("open refused")
	}
	if _, _, ok := s.claimPage(id, now.Add(sessionTTL+time.Second)); ok {
		t.Error("an expired session served its page")
	}

	id, _ = s.open("100", "u1", now)
	s.claimPage(id, now)
	s.setFiles(id, []string{"11"})
	if _, _, ok := s.authorize(id, "11", now.Add(sessionTTL+time.Second)); ok {
		t.Error("an expired session authorised a download")
	}
}

func TestEmptyChannelEndsTheSessionAtOnce(t *testing.T) {
	s := newSessions()
	now := time.Now()
	id, _ := s.open("100", "u1", now)
	s.claimPage(id, now)
	s.setFiles(id, nil)
	if _, _, ok := s.authorize(id, "11", now); ok {
		t.Error("a session with nothing to download stayed open")
	}
}

func TestSessionCap(t *testing.T) {
	s := newSessions()
	now := time.Now()
	for i := 0; i < maxSessions; i++ {
		if _, ok := s.open("100", "u1", now); !ok {
			t.Fatalf("refused session %d before the cap", i)
		}
	}
	if _, ok := s.open("100", "u1", now); ok {
		t.Error("opened a session past the cap")
	}
	if _, ok := s.open("100", "u1", now.Add(sessionTTL+time.Second)); !ok {
		t.Error("expired sessions were not cleared to make room")
	}
}

func TestCourseName(t *testing.T) {
	for in, want := range map[string]string{
		"δομές-δεδομένων":   "Δομές δεδομένων",
		"άλγεβρα":           "Άλγεβρα",
		"λειτουργικά--συστ": "Λειτουργικά συστ",
		"":                  "",
	} {
		if got := courseName(in); got != want {
			t.Errorf("courseName(%q) = %q, want %q", in, got, want)
		}
	}
}
