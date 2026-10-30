// Package shield watches the public HTTP surface for floods and scanning and
// turns on Railway's Under Attack Mode when it sees one.
package shield

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MaterialFilesPrefix is the path of the material page's file downloads,
// which are exempt from the flood count when they succeed.
const MaterialFilesPrefix = "/materials/files/"

const (
	// rateSeconds is the window the request rate is averaged over, so one
	// page load (about four requests) never looks like a flood.
	rateSeconds = 10
	// maxPerSecond is the sustained average above which traffic is treated
	// as automated.
	maxPerSecond = 5

	// maxNotFound is how many 404s are tolerated per notFoundWindow.
	maxNotFound    = 10
	notFoundWindow = time.Hour

	// cooldown matches the duration Under Attack Mode is switched on for, so
	// the detector stays quiet while the protection is still active.
	cooldown = time.Hour
	// retryAfter is how long to wait after a failed attempt to enable it.
	retryAfter = 5 * time.Minute
	// enableTimeout bounds one call to Railway.
	enableTimeout = 30 * time.Second
)

// Enabler turns Under Attack Mode on. reason is for logs and alerts.
type Enabler func(ctx context.Context) error

// Detector counts requests and responses and calls the enabler when traffic
// crosses a threshold.
type Detector struct {
	enable Enabler
	notify func(reason string, err error)
	log    *slog.Logger

	mu         sync.Mutex
	secs       [rateSeconds]second
	notFound   []time.Time
	quietUntil time.Time
}

type second struct {
	at    int64
	count int
}

// New returns a Detector. notify is called after every attempt, with the
// reason and the enabler's error, and may be nil.
func New(enable Enabler, notify func(reason string, err error), logger *slog.Logger) *Detector {
	return &Detector{enable: enable, notify: notify, log: logger}
}

// observe records one finished request and returns why Under Attack Mode
// should go on, or "" if it should not.
func (d *Detector) observe(status int, now time.Time) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	sec := now.Unix()
	slot := &d.secs[sec%rateSeconds]
	if slot.at != sec {
		*slot = second{at: sec}
	}
	slot.count++

	if status == http.StatusNotFound {
		d.notFound = append(d.notFound, now)
	}
	// Drop 404s that left the window; the slice never holds more than
	// maxNotFound+1 live entries because crossing the limit resets it.
	cut := 0
	for cut < len(d.notFound) && now.Sub(d.notFound[cut]) > notFoundWindow {
		cut++
	}
	d.notFound = d.notFound[cut:]

	if now.Before(d.quietUntil) {
		return ""
	}

	total := 0
	for _, s := range d.secs {
		if sec-s.at < rateSeconds {
			total += s.count
		}
	}
	var reason string
	switch {
	case total > maxPerSecond*rateSeconds:
		reason = fmt.Sprintf("more than %d requests/s over %ds (%d requests)", maxPerSecond, rateSeconds, total)
	case len(d.notFound) > maxNotFound:
		reason = fmt.Sprintf("%d 404 responses in the last hour (limit %d)", len(d.notFound), maxNotFound)
	default:
		return ""
	}
	d.quietUntil = now.Add(cooldown)
	d.secs = [rateSeconds]second{}
	d.notFound = nil
	return reason
}

// fire enables the protection and, on failure, lets the detector try again
// soon instead of staying quiet for the full cooldown.
func (d *Detector) fire(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), enableTimeout)
	defer cancel()
	err := d.enable(ctx)
	if err != nil {
		d.mu.Lock()
		d.quietUntil = time.Now().Add(retryAfter)
		d.mu.Unlock()
		d.log.Error("under attack mode: enable failed", "reason", reason, "error", err)
	} else {
		d.log.Warn("under attack mode enabled", "reason", reason)
	}
	if d.notify != nil {
		d.notify(reason, err)
	}
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the connection, which the
// material downloads need to extend their write deadline.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Middleware counts every request except the platform's health probe and
// successful material downloads. Wrap it outside the rate limiter, so
// requests that were turned away still count towards a flood.
//
// One material page fetches every file of its channel at once, so a class
// opening it together easily passes the flood threshold. Those downloads only
// succeed with a valid, recently issued link, which a flood cannot forge;
// refused ones are still counted.
func (d *Detector) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &recorder{ResponseWriter: w}
		defer func() {
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			if status == http.StatusOK && strings.HasPrefix(r.URL.Path, MaterialFilesPrefix) {
				return
			}
			if reason := d.observe(status, time.Now()); reason != "" {
				go d.fire(reason)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}
