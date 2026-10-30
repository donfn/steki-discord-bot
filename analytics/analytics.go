// Package analytics sends the bot's activity to PostHog.
//
// Every event is captured on the server: from the Discord gateway handlers or
// from an HTTP request the bot itself served. Nothing is loaded into a
// visitor's browser, and no event carries message text or an email address.
package analytics

import (
	"fmt"
	"log/slog"

	"github.com/posthog/posthog-go"
)

// system is the distinct ID of events no member performed.
const system = "steki-bot"

// The most events, and batches of them, held in memory while PostHog cannot be
// reached. Further events are dropped.
const (
	maxQueuedEvents  = 1000
	maxQueuedBatches = 20
)

// Client forwards events to PostHog. The zero value and nil are valid and
// drop everything, so callers never check whether analytics is configured.
type Client struct {
	ph posthog.Client
}

// New returns a client for the PostHog project with the given key, or nil
// when key is empty. host is the ingestion endpoint, for example
// https://eu.i.posthog.com.
func New(key, host string, logger *slog.Logger) (*Client, error) {
	if key == "" {
		return nil, nil
	}
	noGeo := true
	ph, err := posthog.NewWithConfig(key, posthog.Config{
		Endpoint: host,
		Logger:   slogLogger{logger},
		// Every request reaches PostHog from this server, so its location
		// says nothing about the member.
		DisableGeoIP: &noGeo,
		// Analytics is best effort. Left alone the library buffers up to
		// 10,000 events and 1,000 batches while PostHog is unreachable, tens
		// of megabytes in a process whose heap is held to 24 MiB; with these
		// limits an outage costs a few thousand events at most, and Capture
		// never blocks.
		MaxQueueSize:        maxQueuedEvents,
		MaxEnqueuedRequests: maxQueuedBatches,
	})
	if err != nil {
		return nil, err
	}
	return &Client{ph: ph}, nil
}

// Capture records event for the member with the Discord ID userID. An empty
// userID records it against no one: the event counts, but PostHog creates no
// person for it.
func (c *Client) Capture(userID, event string, props map[string]any) {
	if c == nil || c.ph == nil {
		return
	}
	p := posthog.NewProperties()
	for k, v := range props {
		p.Set(k, v)
	}
	id := userID
	if id == "" {
		id = system
		p.Set("$process_person_profile", false)
	}
	_ = c.ph.Enqueue(posthog.Capture{DistinctId: id, Event: event, Properties: p})
}

// Close flushes the queued events. It blocks until they are sent or the
// client gives up.
func (c *Client) Close() {
	if c == nil || c.ph == nil {
		return
	}
	_ = c.ph.Close()
}

// slogLogger routes the PostHog library's own messages into the bot's log.
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Debugf(format string, args ...any) {
	s.l.Debug("posthog: " + fmt.Sprintf(format, args...))
}
func (s slogLogger) Logf(format string, args ...any) {
	s.l.Debug("posthog: " + fmt.Sprintf(format, args...))
}
func (s slogLogger) Warnf(format string, args ...any) {
	s.l.Warn("posthog: " + fmt.Sprintf(format, args...))
}
func (s slogLogger) Errorf(format string, args ...any) {
	s.l.Error("posthog: " + fmt.Sprintf(format, args...))
}
