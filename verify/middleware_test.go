package verify

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func hardened() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	return Harden(mux, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHardenHeadersOnNotFoundAndPanic(t *testing.T) {
	h := hardened()
	if w := do(h, "GET", "/nope", nil); w.Code != 404 || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("404 = %d headers %v", w.Code, w.Header())
	}
	if w := do(h, "GET", "/boom", nil); w.Code != 500 {
		t.Fatalf("panic = %d", w.Code)
	}
}

func TestHardenRejectsCrossSiteAndWrongType(t *testing.T) {
	h := hardened()
	json := map[string]string{"Content-Type": "application/json"}
	if w := do(h, "POST", "/x", json); w.Code != 200 {
		t.Fatalf("plain post = %d", w.Code)
	}
	if w := do(h, "POST", "/x", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
		t.Fatalf("cross-site = %d", w.Code)
	}
	if w := do(h, "POST", "/x", map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}); w.Code != 403 {
		t.Fatalf("foreign origin = %d", w.Code)
	}
	if w := do(h, "POST", "/x", map[string]string{"Content-Type": "text/plain"}); w.Code != 415 {
		t.Fatalf("text/plain = %d", w.Code)
	}
}

func TestHardenRateLimitsPerAddress(t *testing.T) {
	h := hardened()
	hdr := map[string]string{"X-Forwarded-For": "1.1.1.1, 9.9.9.9"}
	var got429 bool
	for i := 0; i < rateLimit+5; i++ {
		if do(h, "GET", "/nope", hdr).Code == 429 {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("no 429 after exceeding the limit")
	}
	// A different last hop is a different client; the forged first hop is ignored.
	if w := do(h, "GET", "/nope", map[string]string{"X-Forwarded-For": "1.1.1.1, 8.8.8.8"}); w.Code == 429 {
		t.Fatal("client was limited by a forgeable header entry")
	}
}

func TestHardenGivesTheActivityTheFileBudget(t *testing.T) {
	h := hardened()
	hdr := map[string]string{"X-Forwarded-For": "7.7.7.7"}
	// Discord's proxy puts many members behind one address.
	for i := 0; i < rateLimit*3; i++ {
		if w := do(h, "GET", "/activity/static/x", hdr); w.Code == 429 {
			t.Fatalf("activity request %d limited at the page budget", i)
		}
	}
	// The login, which calls Discord, keeps the page budget.
	var got429 bool
	for i := 0; i < rateLimit+5; i++ {
		if do(h, "POST", "/activity/token", map[string]string{"X-Forwarded-For": "6.6.6.6", "Content-Type": "application/json"}).Code == 429 {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("the activity login is not rate limited at the page budget")
	}
}
