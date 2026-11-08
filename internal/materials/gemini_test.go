package materials

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"steki-bot/internal/store"
)

var testJPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("x"), 5000)...)

func opener(b []byte) Opener {
	return func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

func testGemini(url string) *Gemini {
	g := NewGemini("secret-key", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.url = url
	g.gap = 0
	return g
}

func reply(kind string, people bool) string {
	text, _ := json.Marshal(map[string]any{"kind": kind, "people": people, "reason": "ένα σχήμα"})
	body, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{
		"finishReason": "STOP",
		"content":      map[string]any{"parts": []any{map[string]any{"text": string(text)}}},
	}}})
	return string(body)
}

func TestGeminiVerdicts(t *testing.T) {
	var got struct {
		key  string
		body []byte
	}
	var answer string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.key = r.Header.Get("x-goog-api-key")
		got.body, _ = io.ReadAll(r.Body)
		if r.URL.RawQuery != "" {
			t.Errorf("the key must travel in a header, got query %q", r.URL.RawQuery)
		}
		io.WriteString(w, answer)
	}))
	defer ts.Close()
	g := testGemini(ts.URL)
	m := &store.Material{ContentType: TypeJPEG, Size: int64(len(testJPEG))}

	for _, tc := range []struct {
		name   string
		answer string
		status string
		note   string
	}{
		{"notes", reply("course_material", false), store.MaterialApproved, ""},
		{"meme", reply("meme_or_joke", false), store.MaterialReview, "meme"},
		{"people", reply("course_material", true), store.MaterialReview, "άνθρωποι"},
		{"explicit", reply("explicit_sexual", true), store.MaterialReview, "ακατάλληλο"},
		{"unknown kind", reply("something_new", false), store.MaterialReview, "υλικό μαθήματος"},
		{"blocked prompt", `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`, store.MaterialReview, "αρνήθηκε"},
		{"blocked output", `{"candidates":[{"finishReason":"IMAGE_SAFETY"}]}`, store.MaterialReview, "αρνήθηκε"},
	} {
		answer = tc.answer
		v, err := g.Screen(context.Background(), m, opener(testJPEG))
		if err != nil || v.Status != tc.status || !v.Checked || !strings.Contains(v.Note, tc.note) {
			t.Errorf("%s: verdict %+v, err %v", tc.name, v, err)
		}
	}
	if got.key != "secret-key" {
		t.Errorf("api key header = %q", got.key)
	}
	// The image went out inline, base64-encoded, and intact.
	answer = reply("course_material", false)
	if _, err := g.Screen(context.Background(), m, opener(testJPEG)); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Contents []struct {
			Parts []struct {
				Inline *struct {
					Mime string `json:"mime_type"`
					Data string `json:"data"`
				} `json:"inline_data"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(got.body, &req); err != nil {
		t.Fatalf("request is not JSON: %v", err)
	}
	in := req.Contents[0].Parts[1].Inline
	raw, _ := base64.StdEncoding.DecodeString(in.Data)
	if in.Mime != "image/jpeg" || !bytes.Equal(raw, testJPEG) {
		t.Error("image was not sent intact")
	}
}

func TestGeminiNotAJPEGIsRejectedWithoutACall(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the API was called for a file that is not a JPEG")
	}))
	defer ts.Close()
	v, err := testGemini(ts.URL).Screen(context.Background(),
		&store.Material{ContentType: TypeJPEG, Size: 10}, opener([]byte("not a jpeg at all")))
	if err != nil || v.Status != store.MaterialRejected {
		t.Errorf("verdict %+v, err %v", v, err)
	}
}

func TestGeminiOversizeGoesToReview(t *testing.T) {
	v, err := testGemini("http://unused").Screen(context.Background(),
		&store.Material{ContentType: TypeJPEG, Size: maxInlineImage + 1}, opener(nil))
	if err != nil || v.Status != store.MaterialReview {
		t.Errorf("verdict %+v, err %v", v, err)
	}
}

func TestGeminiQuota(t *testing.T) {
	status := http.StatusTooManyRequests
	var body, header string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if header != "" {
			w.Header().Set("Retry-After", header)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer ts.Close()
	g := testGemini(ts.URL)
	m := &store.Material{ContentType: TypeJPEG, Size: 10}
	wait := func() time.Duration {
		t.Helper()
		_, err := g.Screen(context.Background(), m, opener(testJPEG))
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("err = %v, want a RateLimitError", err)
		}
		return rl.RetryAfter
	}

	body = `{"error":{"message":"quota","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"34s"}]}}`
	if d := wait(); d != 35*time.Second {
		t.Errorf("RetryInfo wait = %v, want 35s", d)
	}
	body, header = `{"error":{"message":"quota"}}`, "90"
	if d := wait(); d != 91*time.Second {
		t.Errorf("Retry-After wait = %v, want 91s", d)
	}
	// No hint at all: the wait doubles from a minute up to an hour.
	header = ""
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if d := wait(); d != want+time.Second {
			t.Errorf("backoff = %v, want %v", d, want+time.Second)
		}
	}
	var last time.Duration
	for range 10 {
		last = wait()
	}
	if last != backoffMax+time.Second {
		t.Errorf("backoff cap = %v, want %v", last, backoffMax+time.Second)
	}
	// A success resets the backoff.
	status, body = http.StatusOK, reply("course_material", false)
	if _, err := g.Screen(context.Background(), m, opener(testJPEG)); err != nil {
		t.Fatal(err)
	}
	status, body = http.StatusTooManyRequests, `{}`
	if d := wait(); d != backoffStart+time.Second {
		t.Errorf("backoff after success = %v", d)
	}
}

func TestGeminiOtherErrorsDoNotPause(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"API key not valid"}}`)
	}))
	defer ts.Close()
	_, err := testGemini(ts.URL).Screen(context.Background(),
		&store.Material{ContentType: TypeJPEG, Size: 10}, opener(testJPEG))
	var rl *RateLimitError
	if err == nil || errors.As(err, &rl) || !strings.Contains(err.Error(), "API key not valid") {
		t.Errorf("err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "secret-key") {
		t.Error("the error leaks the API key")
	}
}

func TestGeminiSpacesRequests(t *testing.T) {
	g := testGemini("http://unused")
	g.gap = 50 * time.Millisecond
	start := time.Now()
	for range 3 {
		if err := g.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 90*time.Millisecond {
		t.Errorf("three calls took %v, want them spaced by the gap", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.wait(ctx); err == nil {
		t.Error("wait ignored a cancelled context")
	}
}

func TestGeminiPDFsGetTheFormatCheckOnly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the API was called for a PDF")
	}))
	defer ts.Close()
	v, err := testGemini(ts.URL).Screen(context.Background(),
		&store.Material{ContentType: TypePDF}, opener([]byte("%PDF-1.7 rest")))
	if err != nil || v.Status != store.MaterialApproved || v.Checked {
		t.Errorf("verdict %+v, err %v", v, err)
	}
}
