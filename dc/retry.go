// Package dc holds Discord helpers that sit between the bot's event handlers
// and discordgo: bounded retries, error classification, and register-channel
// placement.
package dc

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
)

// maxAttempts bounds every retried Discord call.
const maxAttempts = 4

// Do runs fn, retrying transient failures with exponential backoff and jitter.
//
// A 4xx other than 429 is returned immediately: those are deterministic (bad
// permissions, a full category, a deleted channel) and retrying only delays the
// alert the moderators need to see.
func Do(ctx context.Context, log *slog.Logger, op string, fn func() error) error {
	var err error
	backoff := 500 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = fn()
		if err == nil {
			if attempt > 1 {
				log.Info("discord call recovered", "op", op, "attempt", attempt)
			}
			return nil
		}
		if !retryable(err) || attempt == maxAttempts {
			return err
		}

		wait := backoff + time.Duration(rand.Int64N(int64(250*time.Millisecond)))
		if after, ok := retryAfter(err); ok {
			wait = after
		}

		log.Warn("retrying discord call",
			"op", op, "attempt", attempt, "wait", wait, "error", err)

		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
		backoff *= 2
	}
	return err
}

// RESTCode returns the HTTP status of a discordgo REST error, or 0.
func RESTCode(err error) int {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil {
		return restErr.Response.StatusCode
	}
	return 0
}

// APICode returns Discord's own numeric error code (e.g. 50035 for an invalid
// form body), or 0 when the error carries none.
func APICode(err error) int {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Message != nil {
		return int(restErr.Message.Code)
	}
	return 0
}

// IsMissingResource reports whether Discord says the target no longer exists,
// which the reconciler treats as "already cleaned up" rather than a failure.
func IsMissingResource(err error) bool {
	return RESTCode(err) == http.StatusNotFound ||
		APICode(err) == 10003 || // unknown channel
		APICode(err) == 10007 || // unknown member
		APICode(err) == 10013 // unknown user
}

func retryable(err error) bool {
	status := RESTCode(err)
	switch {
	case status == http.StatusTooManyRequests:
		return true
	case status >= 500:
		return true
	case status >= 400:
		return false
	default:
		// No REST status: a transport-level failure, which is worth retrying.
		return true
	}
}

func retryAfter(err error) (time.Duration, bool) {
	var rl *discordgo.RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return rl.RetryAfter, true
	}
	return 0, false
}
