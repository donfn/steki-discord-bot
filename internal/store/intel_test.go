package store

import (
	"context"
	"errors"
	"testing"
)

func TestIntelFlow(t *testing.T) {
	ctx := context.Background()
	s := openTestGuard(t)

	id, added, err := s.AddIntel(ctx, IntelLink, "evil.test/gift", "evil.test/gift", "u1")
	if err != nil || !added {
		t.Fatalf("add = %v, %v", added, err)
	}
	again, added, err := s.AddIntel(ctx, IntelLink, "evil.test/gift", "ignored", "u2")
	if err != nil || added || again != id {
		t.Fatalf("second add = %d, %v, %v; want %d, false", again, added, err, id)
	}
	// The same key under the other kind is a different item.
	if other, added, _ := s.AddIntel(ctx, IntelMedia, "evil.test/gift", "x.png", "u1"); !added || other == id {
		t.Fatal("kinds share keys")
	}

	e, err := s.MatchIntel(ctx, IntelLink, "evil.test/gift")
	if err != nil || e.ID != id || e.Label != "evil.test/gift" || e.UserID != "u1" {
		t.Fatalf("match = %+v, %v", e, err)
	}
	if _, err := s.MatchIntel(ctx, IntelLink, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown item: %v", err)
	}
	if err := s.CountIntelHit(ctx, id); err != nil {
		t.Fatal(err)
	}

	gone, err := s.RemoveIntel(ctx, id)
	if err != nil || gone.Kind != IntelLink {
		t.Fatalf("remove = %+v, %v", gone, err)
	}
	if _, err := s.MatchIntel(ctx, IntelLink, "evil.test/gift"); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed entry still matches")
	}
	if _, err := s.RemoveIntel(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
}
