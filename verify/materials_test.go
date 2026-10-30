package verify

import (
	"context"
	"io"
	"net/http"
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

func get(t *testing.T, url, bearer string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestMaterialsPageLists(t *testing.T) {
	e := newEnv(t)
	res, body := get(t, e.srv.URL+"/materials?t="+e.materialToken(t, "100", time.Minute), "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	for _, want := range []string{"μαθηματικά-ι", "θέματα.pdf", `"id":"11"`, "pdfjs-dist@4.10.38"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
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

func TestMaterialsPageRefusesOtherTokens(t *testing.T) {
	e := newEnv(t)
	guard, _ := e.tk.Issue("u1", "g1", ReasonMedia, "jti1", time.Now().Add(time.Minute))
	link, _ := e.tk.IssueLink("u1", "g1", "link1", time.Now().Add(time.Minute))
	for name, tok := range map[string]string{
		"guard":    guard,
		"link":     link,
		"expired":  e.materialToken(t, "100", -time.Minute),
		"no page":  e.materialToken(t, "999", time.Minute),
		"garbage":  "abc",
		"no token": "",
	} {
		_, body := get(t, e.srv.URL+"/materials?t="+tok, "")
		if !strings.Contains(body, "Ο σύνδεσμος δεν ισχύει πια") || strings.Contains(body, `"files"`) {
			t.Errorf("%s token: page did not show the invalid state", name)
		}
	}
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
	tok := e.materialToken(t, "100", time.Minute)

	res, body := get(t, e.srv.URL+"/materials/files/11", tok)
	if res.StatusCode != http.StatusOK || body != "%PDF-1.7" || res.Header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("pdf: status %d type %q body %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	res, _ = get(t, e.srv.URL+"/materials/files/12", tok)
	if res.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("jpeg served as %q", res.Header.Get("Content-Type"))
	}

	// Another channel's file is not reachable with this channel's link.
	if res, _ := get(t, e.srv.URL+"/materials/files/21", tok); res.StatusCode != http.StatusGone {
		t.Errorf("cross-channel file: status %d, want 410", res.StatusCode)
	}
	// The token is only accepted as a bearer header.
	if res, _ := get(t, e.srv.URL+"/materials/files/11?t="+tok, ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("token in query: status %d, want 401", res.StatusCode)
	}
	if res, _ := get(t, e.srv.URL+"/materials/files/11", e.materialToken(t, "100", -time.Second)); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired token: status %d, want 401", res.StatusCode)
	}
	if res, _ := get(t, e.srv.URL+"/materials/files/..%2f11", tok); res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id: status %d, want 400", res.StatusCode)
	}
}
