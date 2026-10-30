package store

import (
	"context"
	"testing"
	"time"
)

func TestIPBans(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)

	add := func(ip, kind string, start time.Time, d time.Duration) {
		t.Helper()
		if err := s.RecordIPBan(ctx, IPBan{IP: ip, Kind: kind, Reason: "test", StartedAt: start, Until: start.Add(d)}); err != nil {
			t.Fatal(err)
		}
	}
	add("192.0.2.1", IPChallenge, now.Add(-30*time.Minute), time.Hour)
	add("192.0.2.1", IPBanned, now.Add(-10*time.Minute), 6*time.Hour)
	add("192.0.2.2", IPChallenge, now.Add(-2*time.Hour), time.Hour) // over

	got, err := s.ActiveIPBans(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != IPChallenge || got[1].Kind != IPBanned || got[1].IP != "192.0.2.1" {
		t.Fatalf("active blocks: %+v", got)
	}
	if !got[1].Until.Equal(now.Add(-10 * time.Minute).Add(6 * time.Hour)) {
		t.Fatalf("until = %v", got[1].Until)
	}

	// A new block prunes the ones that ended past the history.
	add("192.0.2.3", IPChallenge, now.Add(-ipBanHistory-2*time.Hour), time.Hour)
	add("192.0.2.4", IPChallenge, now, time.Hour)
	var n int
	if err := s.db.QueryRowContext(ctx, `select count(*) from ip_bans where ip = '192.0.2.3'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("old block kept: %d rows", n)
	}
}
