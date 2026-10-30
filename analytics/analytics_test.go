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
	c.Capture("1", "x", nil)
	c.Close()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if c.Middleware(h) == nil {
		t.Fatal("nil client must pass the handler through")
	}
}

func TestCaptureWithoutMemberCreatesNoPerson(t *testing.T) {
	f := &fake{}
	c := &Client{ph: f}
	c.Capture("", "e", nil)
	c.Capture("42", "e", nil)
	if f.got[0].DistinctId != system || f.got[0].Properties["$process_person_profile"] != false {
		t.Fatalf("anonymous event = %+v", f.got[0])
	}
	if f.got[1].DistinctId != "42" || f.got[1].Properties["$process_person_profile"] != nil {
		t.Fatalf("member event = %+v", f.got[1])
	}
}

func TestMiddlewareRecordsRouteStatusAndTags(t *testing.T) {
	f := &fake{}
	c := &Client{ph: f}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /materials/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		Tag(r.Context(), "42", "channel_id", "7")
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
	if e.DistinctId != "42" || e.Event != "http_request" {
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
}

func TestTagOutsideRequestIsNoop(t *testing.T) {
	Tag(t.Context(), "42", "k", "v")
}
