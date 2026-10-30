package shield

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTest() *Detector {
	return New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestSustainedRateTriggers(t *testing.T) {
	d := newTest()
	now := time.Unix(1000, 0)
	// 5 req/s for 10s is at the limit, not over it.
	for i := 0; i < 50; i++ {
		if r := d.observe(200, now.Add(time.Duration(i)*200*time.Millisecond)); r != "" {
			t.Fatalf("request %d triggered at exactly 5/s: %s", i, r)
		}
	}
	if r := d.observe(200, now.Add(9*time.Second+900*time.Millisecond)); r == "" {
		t.Fatal("51st request in 10s did not trigger")
	}
}

func TestPageLoadBurstDoesNotTrigger(t *testing.T) {
	d := newTest()
	now := time.Unix(1000, 0)
	for i := 0; i < 20; i++ { // well above 5/s for a moment, but not sustained
		if r := d.observe(200, now); r != "" {
			t.Fatalf("burst of %d requests triggered: %s", i+1, r)
		}
	}
}

func TestNotFoundThreshold(t *testing.T) {
	d := newTest()
	now := time.Unix(1000, 0)
	for i := 0; i < maxNotFound; i++ {
		if r := d.observe(404, now.Add(time.Duration(i)*time.Minute)); r != "" {
			t.Fatalf("404 #%d triggered: %s", i+1, r)
		}
	}
	if r := d.observe(404, now.Add(10*time.Minute)); !strings.Contains(r, "404") {
		t.Fatalf("11th 404 in an hour: reason %q", r)
	}
}

func TestOldNotFoundsExpire(t *testing.T) {
	d := newTest()
	now := time.Unix(1000, 0)
	for i := 0; i < maxNotFound; i++ {
		d.observe(404, now)
	}
	if r := d.observe(404, now.Add(notFoundWindow+time.Minute)); r != "" {
		t.Fatalf("404s from over an hour ago still counted: %s", r)
	}
}

func TestQuietAfterTrigger(t *testing.T) {
	d := newTest()
	now := time.Unix(1000, 0)
	for i := 0; i <= maxNotFound; i++ {
		d.observe(404, now)
	}
	for i := 0; i <= maxNotFound; i++ {
		if r := d.observe(404, now.Add(time.Minute)); r != "" {
			t.Fatalf("retriggered during cooldown: %s", r)
		}
	}
	// Back to normal once the hour of protection is over.
	later := now.Add(cooldown + time.Minute)
	var fired bool
	for i := 0; i <= maxNotFound; i++ {
		fired = d.observe(404, later) != "" || fired
	}
	if !fired {
		t.Fatal("did not trigger again after the cooldown")
	}
}

func TestMiddlewareFiresAndRetriesAfterFailure(t *testing.T) {
	calls := make(chan struct{}, 4)
	notified := make(chan error, 4)
	d := New(func(context.Context) error {
		calls <- struct{}{}
		return errors.New("boom")
	}, func(_ string, err error) { notified <- err }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := d.Middleware(http.NotFoundHandler())
	for i := 0; i <= maxNotFound; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/wp-login.php", nil))
	}
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("enabler not called")
	}
	if err := <-notified; err == nil {
		t.Fatal("failure not reported")
	}
	d.mu.Lock()
	wait := time.Until(d.quietUntil)
	d.mu.Unlock()
	if wait > retryAfter || wait < retryAfter-time.Minute {
		t.Fatalf("after a failure the detector should retry in ~%s, waits %s", retryAfter, wait)
	}
}

func TestHealthzIsNotCounted(t *testing.T) {
	d := newTest()
	h := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	for i := 0; i < 100; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.notFound) != 0 || d.secs != [rateSeconds]second{} {
		t.Fatal("health probes were counted")
	}
}

func TestRailwayEnablerArgs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "railway")
	script := "#!/bin/sh\necho \"$@\" > " + out + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RailwayEnabler(bin, "svc-123")(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if want := "waf under-attack enable --duration 1h --service svc-123"; strings.TrimSpace(string(got)) != want {
		t.Fatalf("args %q, want %q", got, want)
	}
	if err := RailwayEnabler(filepath.Join(dir, "missing"), "")(context.Background()); err == nil {
		t.Fatal("missing binary should fail")
	}
}
