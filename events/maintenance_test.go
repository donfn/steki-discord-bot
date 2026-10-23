package events

import (
	"strconv"
	"testing"
	"time"
)

// snowflakeAt builds a Discord ID whose embedded timestamp is t.
func snowflakeAt(t time.Time) string {
	const discordEpochMs = 1420070400000
	return strconv.FormatInt((t.UnixMilli()-discordEpochMs)<<22, 10)
}

func TestCreatedAfter(t *testing.T) {
	now := time.Now()
	if createdAfter(snowflakeAt(now.Add(-time.Hour)), now) {
		t.Error("an hour-old channel was treated as new")
	}
	if !createdAfter(snowflakeAt(now.Add(time.Minute)), now) {
		t.Error("a newer channel was not treated as new")
	}
	if !createdAfter("not-a-snowflake", now) {
		t.Error("an unparseable ID must count as new so it is left alone")
	}
}
