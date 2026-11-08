// Package analytics sends the bot's activity to PostHog.
//
// Every event is captured on the server: from the Discord gateway handlers or
// from an HTTP request the bot itself served. Nothing is loaded into a
// visitor's browser.
//
// The events only count interactions. They never say who: no Discord ID,
// username, email address, message text or IP address leaves the bot, and
// every event is recorded against one shared anonymous ID with no person
// profile. Capture enforces this by dropping any property that is not a
// number, a flag or one of the known short labels in labelKeys, so a caller
// cannot leak by passing the wrong thing.
package analytics

import (
	"fmt"
	"log/slog"

	"github.com/posthog/posthog-go"
)

// anonymous is the distinct ID of every event. It identifies nobody.
const anonymous = "steki-bot"

// labelKeys are the only properties whose string values are sent: short fixed
// labels (a route, a command, a step) and Discord's guild and channel IDs,
// which name places rather than members. Numbers and flags are always sent.
var labelKeys = map[string]bool{
	"guild_id": true, "channel_id": true, "kind": true, "command": true,
	"custom_id": true, "action": true, "step": true, "outcome": true,
	"page": true, "state": true, "university": true, "tool": true,
	"mcp_method": true, "method": true, "route": true, "captcha": true,
	"emoji": true,
}

const maxLabel = 64

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

// Capture records one event. props are filtered: see the package comment.
func (c *Client) Capture(event string, props map[string]any) {
	if c == nil || c.ph == nil {
		return
	}
	p := posthog.NewProperties()
	for k, v := range props {
		if v, ok := clean(k, v); ok {
			p.Set(k, v)
		}
	}
	// No person profile: the event counts, but PostHog creates no person.
	p.Set("$process_person_profile", false)
	_ = c.ph.Enqueue(posthog.Capture{DistinctId: anonymous, Event: event, Properties: p})
}

// clean returns v if it may be sent as property k.
func clean(k string, v any) (any, bool) {
	switch v := v.(type) {
	case bool, int, int32, int64, uint, uint32, uint64, float32, float64:
		return v, true
	case string:
		if labelKeys[k] {
			return clip(v, maxLabel), true
		}
	case []string:
		// The content types of a message's attachments.
		if k == "attachment_types" {
			out := make([]string, len(v))
			for i, t := range v {
				out[i] = clip(t, maxLabel)
			}
			return out, true
		}
	}
	return nil, false
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
