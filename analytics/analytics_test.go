package analytics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/posthog/posthog-go"
)

// fake collects what would have been sent to PostHog.
type fake struct {
	posthog.Client
	got []posthog.Capture
}

func (f *fake) Enqueue(m posthog.Message) error {
	f.got = append(f.got, m.(posthog.Capture))
	return nil
}

func TestNilClientDropsEverything(t *testing.T) {
	var c *Client
	c.Capture("x", nil)
	c.Close()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if c.Middleware(h) == nil {
		t.Fatal("nil client must pass the handler through")
	}
}

func TestCaptureIsAnonymousAndDropsAnythingThatCouldIdentify(t *testing.T) {
	f := &fake{}
	c := &Client{ph: f}
	c.Capture("e", map[string]any{
		"channel_id":       "7",
		"count":            3,
		"banned":           true,
		"attachment_types": []string{"image/png"},
		"username":         "someone",
		"email":            "someone@hmu.gr",
		"reason":           "free text",
		"title":            "free text",
		"user_id":          "42",
		"owner":            "42",
		"hashes":           []string{"abc"},
		"command":          strings.Repeat("x", 500),
	})
	e := f.got[0]
	if e.DistinctId != anonymous || e.Properties["$process_person_profile"] != false {
		t.Fatalf("event is not anonymous: %+v", e)
	}
	for _, k := range []string{"username", "email", "reason", "title", "user_id", "owner", "hashes"} {
		if _, leaked := e.Properties[k]; leaked {
			t.Errorf("property %q was sent", k)
		}
	}
	p := e.Properties
	if p["channel_id"] != "7" || p["count"] != 3 || p["banned"] != true {
		t.Fatalf("properties = %+v", p)
	}
	if got := p["command"].(string); len(got) != maxLabel {
		t.Fatalf("label not clipped: %d bytes", len(got))
	}
	if types, _ := p["attachment_types"].([]string); len(types) != 1 {
		t.Fatalf("attachment types lost: %+v", p)
	}
}

func TestMiddlewareRecordsRouteStatusAndTags(t *testing.T) {
	f := &fake{}
	c := &Client{ph: f}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /materials/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		Tag(r.Context(), "channel_id", "7")
		w.WriteHeader(http.StatusGone)
	})
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	h := c.Middleware(mux)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/materials/files/9?t=secret-token", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/wp-login.php", nil))

	if len(f.got) != 1 {
		t.Fatalf("got %d events, want 1 (health and unrouted requests are skipped)", len(f.got))
	}
	e := f.got[0]
	if e.DistinctId != anonymous || e.Event != "http_request" {
		t.Fatalf("event = %+v", e)
	}
	p := e.Properties
	if p["route"] != "GET /materials/files/{id}" || p["status"] != http.StatusGone || p["channel_id"] != "7" {
		t.Fatalf("properties = %+v", p)
	}
	for k, v := range p {
		if s, ok := v.(string); ok && strings.Contains(s, "secret-token") {
			t.Fatalf("property %s leaks the query string: %q", k, s)
		}
	}
	for _, k := range []string{"path", "user_agent"} {
		if _, ok := p[k]; ok {
			t.Fatalf("property %s should not be sent", k)
		}
	}
}

func TestTagOutsideRequestIsNoop(t *testing.T) {
	Tag(t.Context(), "k", "v")
}
