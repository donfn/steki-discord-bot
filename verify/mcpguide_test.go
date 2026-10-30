package verify

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPGuidePage(t *testing.T) {
	tk := NewTokens("test-secret-test-secret-test-secret!")
	s := NewServer(tk, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableMCPGuide("https://bot.example/mcp")
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	page := func(token string) (int, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + MCPGuidePath + "?t=" + token)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	issue := func(f func() (string, error)) string {
		t.Helper()
		tok, err := f()
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	hour := time.Now().Add(time.Hour)

	guide := issue(func() (string, error) { return tk.IssueGuide("u1", "1", "a", hour) })
	if code, body := page(guide); code != http.StatusOK || !strings.Contains(body, "https://bot.example/mcp") ||
		!strings.Contains(body, "Add custom connector") {
		t.Errorf("valid link: %d, page missing its content", code)
	}
	// A personal link opens once.
	if _, body := page(guide); !strings.Contains(body, "Το link έληξε") {
		t.Error("a followed guide link opened again")
	}

	// Expired, malformed and other kinds of links all show the expired page.
	expired := issue(func() (string, error) { return tk.IssueGuide("u1", "1", "b", time.Now().Add(-time.Hour)) })
	materials := issue(func() (string, error) { return tk.IssueMaterials("u1", "1", "2", "c", Profile{}, hour) })
	media := issue(func() (string, error) { return tk.Issue("3", "1", ReasonMedia, "d", hour) })
	for name, tok := range map[string]string{"expired": expired, "materials": materials, "media guard": media, "garbage": "x"} {
		if _, body := page(tok); !strings.Contains(body, "Το link έληξε") || strings.Contains(body, "Add custom connector") {
			t.Errorf("%s link was accepted", name)
		}
	}

	// And the reverse: the guide link cannot be used as a media guard link.
	if c, ok := s.guardClaims(guide); ok {
		t.Errorf("guide link accepted as a guard link: %+v", c)
	}
}
