package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFlagTrips(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }

	if FlagTrips(nil, now, time.Hour) {
		t.Error("first flagged post tripped")
	}
	if !FlagTrips(at(59*time.Minute), now, time.Hour) {
		t.Error("second post inside the hour did not trip")
	}
	if FlagTrips(at(61*time.Minute), now, time.Hour) {
		t.Error("post after the window closed tripped")
	}
}

// TestMediaGuardFlow runs against a real, throwaway SQLite database.
func TestMediaGuardFlow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Now()
	post := func(user, token string, at time.Time) bool {
		t.Helper()
		tripped, err := s.RecordFlaggedPost(ctx, user, "g1", token, at, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tripped
	}
	active := func(user, token string) bool {
		t.Helper()
		ok, err := s.GuardTokenActive(ctx, user, token)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if post("u1", "a", t0) {
		t.Fatal("first post tripped")
	}
	if !post("u1", "a", t0.Add(30*time.Minute)) {
		t.Fatal("second post within the hour did not trip")
	}
	if !active("u1", "a") {
		t.Fatal("token not active after trip")
	}
	if err := s.SetGuardDM(ctx, "u1", "a", GuardDM{ChannelID: "c", MessageID: "m"}); err != nil {
		t.Fatal(err)
	}

	dm, err := s.ConsumeGuardToken(ctx, "u1", "a", t0.Add(24*time.Hour))
	if err != nil || dm.ChannelID != "c" || dm.MessageID != "m" {
		t.Fatalf("consume = %+v, %v", dm, err)
	}
	if _, err := s.ConsumeGuardToken(ctx, "u1", "a", t0.Add(24*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume = %v, want ErrNotFound", err)
	}

	// Exempt for a day after solving: any number of posts pass.
	for i := range 3 {
		if post("u1", "b", t0.Add(time.Hour+time.Duration(i)*time.Minute)) {
			t.Fatal("tripped during exemption")
		}
	}

	// A moderator can end the exemption early, once.
	if g, err := s.GuardStatus(ctx, "u1"); err != nil || g.ExemptUntil == nil || g.LinkPending {
		t.Fatalf("status during exemption = %+v, %v", g, err)
	}
	if cleared, err := s.ClearExemption(ctx, "u1"); err != nil || !cleared {
		t.Fatalf("clear exemption = %v, %v", cleared, err)
	}
	if cleared, _ := s.ClearExemption(ctx, "u1"); cleared {
		t.Fatal("cleared an exemption that was already gone")
	}
	if post("u1", "b2", t0.Add(90*time.Minute)) {
		t.Fatal("first post after clearing tripped")
	}
	if _, err := s.GuardStatus(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("status for unseen member = %v", err)
	}

	// A forced restriction applies even during the exemption.
	if err := s.ForceRestriction(ctx, "u1", "g1", "forced", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !active("u1", "forced") {
		t.Fatal("forced restriction did not record its link")
	}
	if err := s.ForceRestriction(ctx, "new-user", "g1", "f2", t0); err != nil || !active("new-user", "f2") {
		t.Fatalf("forced restriction for an unseen member: %v", err)
	}

	// Posts an hour apart never trip.
	if post("u2", "x", t0) || post("u2", "x", t0.Add(61*time.Minute)) {
		t.Fatal("posts more than an hour apart tripped")
	}

	// A new trip supersedes the previous link.
	post("u3", "c1", t0)
	post("u3", "c1", t0.Add(time.Minute))
	post("u3", "c2", t0.Add(2*time.Hour))
	post("u3", "c2", t0.Add(2*time.Hour+time.Minute))
	if active("u3", "c1") || !active("u3", "c2") {
		t.Fatal("newer trip did not supersede the old link")
	}
}
