package verify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeReporter lets the tests choose what a report comes to.
type fakeReporter struct {
	*fakeMaterials
	got []string // "user/channel/file/reason"
	res *ReportResult
	err error
}

func (f *fakeReporter) Report(_ context.Context, userID, channelID, fileID, reason string) (*ReportResult, error) {
	f.got = append(f.got, userID+"/"+channelID+"/"+fileID+"/"+reason)
	return f.res, f.err
}

// newReportEnv is newEnv with a material source that takes reports.
func newReportEnv(t *testing.T) (*testEnv, *fakeReporter) {
	t.Helper()
	e := &testEnv{tk: NewTokens("secret"), fl: &fakeLifter{active: "jti1"}, fc: &fakeConsenter{active: "link1"},
		fm: newFakeMaterials()}
	fr := &fakeReporter{fakeMaterials: e.fm, res: &ReportResult{Pulled: true, Note: "δεν είναι υλικό", Left: 4}}
	mux := http.NewServeMux()
	NewServer(e.tk, e.fl, e.fc, fr, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e, fr
}

func postReport(t *testing.T, c *http.Client, base, body string) (*http.Response, map[string]any) {
	t.Helper()
	res, err := c.Post(base+"/s/report", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestReportEndpoint(t *testing.T) {
	e, fr := newReportEnv(t)
	c, _, _ := e.open(t, "100")
	reason := `αυτό είναι ένα meme, όχι υλικό`

	res, out := postReport(t, c, e.srv.URL, `{"file":"11","reason":"`+reason+`"}`)
	if res.StatusCode != http.StatusOK || out["status"] != "received" || out["note"] != nil || out["left"] != float64(4) {
		t.Fatalf("pulled report: %d %v", res.StatusCode, out)
	}
	if len(fr.got) != 1 || fr.got[0] != "u1/100/11/"+reason {
		t.Fatalf("reporter got %v", fr.got)
	}

	fr.res = &ReportResult{Note: "το αρχείο είναι κανονικό υλικό", Left: 3}
	if _, out := postReport(t, c, e.srv.URL, `{"file":"12","reason":"`+reason+`"}`); out["status"] != "received" || out["note"] != nil {
		t.Fatalf("declined report: %v", out)
	}

	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"already":     {ErrReportAlready, http.StatusConflict, "already"},
		"unavailable": {ErrReportUnavailable, http.StatusServiceUnavailable, "unavailable"},
		"gone":        {ErrNoMaterial, http.StatusGone, "gone"},
		"limit":       {&ReportLimitError{ResetsAt: time.Now().Add(time.Hour)}, http.StatusTooManyRequests, "limit"},
	} {
		fr.err = tc.err
		res, out := postReport(t, c, e.srv.URL, `{"file":"11","reason":"`+reason+`"}`)
		if res.StatusCode != tc.status || out["error"] != tc.code {
			t.Errorf("%s: %d %v", name, res.StatusCode, out)
		}
		if name == "limit" && out["resets"] == nil {
			t.Error("limit answer lacks the reset time")
		}
	}
}

func TestReportEndpointRefuses(t *testing.T) {
	e, fr := newReportEnv(t)
	reason := `αυτό είναι ένα meme, όχι υλικό`

	// No session.
	if res, _ := postReport(t, browser(t), e.srv.URL, `{"file":"11","reason":"`+reason+`"}`); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %d", res.StatusCode)
	}
	c, _, _ := e.open(t, "100")
	for name, body := range map[string]string{
		"not a file id": `{"file":"../x","reason":"` + reason + `"}`,
		"too short":     `{"file":"11","reason":"κακό"}`,
		"blank":         `{"file":"11","reason":"            "}`,
		"too long":      `{"file":"11","reason":"` + strings.Repeat("α", reasonMax+1) + `"}`,
		"not json":      `reason`,
	} {
		if res, _ := postReport(t, c, e.srv.URL, body); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, res.StatusCode)
		}
	}
	// A file that was not on the page of this session.
	if res, _ := postReport(t, c, e.srv.URL, `{"file":"21","reason":"`+reason+`"}`); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("file of another channel: %d", res.StatusCode)
	}
	if len(fr.got) != 0 {
		t.Errorf("refused reports reached the reporter: %v", fr.got)
	}
}

func TestReportsWorkAfterTheDownloadsAreDone(t *testing.T) {
	e, _ := newReportEnv(t)
	c, _, _ := e.open(t, "100")
	for _, id := range []string{"11", "12"} {
		fetch(t, c, e.srv.URL+"/s/f/"+id)
	}
	// The session no longer downloads, but the member can still report.
	if res, _ := fetch(t, c, e.srv.URL+"/s/f/11"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("download after completion: %d", res.StatusCode)
	}
	res, out := postReport(t, c, e.srv.URL, `{"file":"11","reason":"αυτό είναι ένα meme, όχι υλικό"}`)
	if res.StatusCode != http.StatusOK || out["status"] != "received" {
		t.Fatalf("report after completion: %d %v", res.StatusCode, out)
	}
}

func TestReportsStopWhenTheSessionRunsOut(t *testing.T) {
	s := newSessions()
	now := time.Now()
	id, _ := s.open("100", "u1", "g1", Profile{}, now)
	s.claimPage(id, now)
	s.setFiles(id, []string{"11"})
	if _, _, ok := s.authorizeOnPage(id, "11", now); !ok {
		t.Fatal("a live session cannot report")
	}
	if _, _, ok := s.authorizeOnPage(id, "12", now); ok {
		t.Error("reported a file the page did not list")
	}
	if _, _, ok := s.authorizeOnPage(id, "11", now.Add(studyTTL+time.Second)); ok {
		t.Error("an expired session reported")
	}
}

func TestViewAsIsReadOnly(t *testing.T) {
	e, fr := newReportEnv(t)
	_ = e.fm.SaveNote(context.Background(), "u2", "11", NoteStudy, json.RawMessage(`{"t":"Οι σημειώσεις του μέλους"}`))
	who := Profile{Name: "Νίκος", Username: "nikos", Viewer: "staff1"}
	tok, err := e.tk.IssueMaterials("u2", "g1", "100", "view1", who, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	c := browser(t)
	res, body := fetch(t, c, e.srv.URL+"/materials?t="+tok)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Προβολή ως") || !strings.Contains(body, "Οι σημειώσεις του μέλους") {
		t.Fatalf("view-as page: %d, missing the member's view", res.StatusCode)
	}
	if socket, _ := pageData(t, body); socket != "" {
		t.Error("a view-as page got a socket token: it could save and be counted")
	}
	if res, _ := postReport(t, c, e.srv.URL, `{"file":"11","reason":"αυτό είναι ένα meme, όχι υλικό"}`); res.StatusCode != http.StatusUnauthorized || len(fr.got) != 0 {
		t.Errorf("view-as report: status %d, %d calls; want refused", res.StatusCode, len(fr.got))
	}
}
