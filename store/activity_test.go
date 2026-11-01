package store

import (
	"context"
	"testing"
	"time"
)

func TestSyncMemberActivity(t *testing.T) {
	ctx := context.Background()
	s := openTestActivity(t)

	joined := time.Now().Add(-200 * 24 * time.Hour).Truncate(time.Millisecond)
	older := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Millisecond)
	newer := time.Now().Add(-2 * 24 * time.Hour).Truncate(time.Millisecond)

	get := func() map[string]MemberActivity {
		t.Helper()
		rows, err := s.MemberActivities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]MemberActivity{}
		for _, r := range rows {
			out[r.UserID] = r
		}
		return out
	}

	err := s.SyncMemberActivity(ctx, time.Now().Add(-time.Minute), []ActivityEntry{
		{UserID: "a", JoinedAt: joined, LastMessageAt: &newer},
		{UserID: "b", JoinedAt: joined},
		{UserID: "gone", JoinedAt: joined},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := get(); len(got) != 3 || got["b"].LastMessageAt != nil {
		t.Fatalf("first sync = %+v", got)
	}
	if err := s.SetGhost(ctx, "b", true); err != nil {
		t.Fatal(err)
	}

	// A later sync keeps the later last message whichever side holds it, keeps
	// the ghost flag, and drops the member the list no longer names.
	time.Sleep(2 * time.Millisecond)
	listedAt := time.Now()
	time.Sleep(2 * time.Millisecond)
	err = s.SyncMemberActivity(ctx, listedAt, []ActivityEntry{
		{UserID: "a", JoinedAt: joined, LastMessageAt: &older},
		{UserID: "b", JoinedAt: joined, LastMessageAt: &older},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := get()
	if _, ok := got["gone"]; ok {
		t.Error("member missing from the list was kept")
	}
	if a := got["a"]; a.LastMessageAt == nil || !a.LastMessageAt.Equal(newer) {
		t.Errorf("a last message = %v, want the later %v", a.LastMessageAt, newer)
	}
	if b := got["b"]; b.LastMessageAt == nil || !b.LastMessageAt.Equal(older) || !b.Ghost {
		t.Errorf("b = %+v, want last message %v and ghost kept", b, older)
	}
}

// TestSyncKeepsRowsWrittenDuringTheListing checks the listedAt guard: a row
// newer than the moment the list was fetched cannot be named by it and must
// survive.
func TestSyncKeepsRowsWrittenDuringTheListing(t *testing.T) {
	ctx := context.Background()
	s := openTestActivity(t)
	joined := time.Now().Add(-time.Hour)

	if err := s.SyncMemberActivity(ctx, time.Now().Add(-time.Minute), []ActivityEntry{
		{UserID: "late", JoinedAt: joined},
	}); err != nil {
		t.Fatal(err)
	}
	// The list below was fetched before "late" was written, and omits them.
	if err := s.SyncMemberActivity(ctx, time.Now().Add(-time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	rows, err := s.MemberActivities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the late row kept", rows)
	}
}

func TestActivityEvents(t *testing.T) {
	ctx := context.Background()
	s := openTestActivity(t)
	now := time.Now()
	old := now.AddDate(0, -5, 0)
	cutoff := now.AddDate(0, -3, 0)

	// A silent old member is a candidate; a newcomer and a recent poster are not.
	for id, joined := range map[string]time.Time{"silent": old, "new": now, "poster": old} {
		if err := s.RecordJoin(ctx, id, joined); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordMessage(ctx, "poster", now, old, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.GhostCandidates(ctx, cutoff, 10)
	if err != nil || len(got) != 1 || got[0] != "silent" {
		t.Fatalf("candidates = %v, %v; want [silent]", got, err)
	}

	// Once flagged they are no longer a candidate; a message with the role still
	// held keeps the flag until the caller clears it, one without clears it.
	if err := s.SetGhost(ctx, "silent", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GhostCandidates(ctx, cutoff, 10); len(got) != 0 {
		t.Fatalf("flagged member still a candidate: %v", got)
	}
	flag := func() bool {
		rows, _ := s.MemberActivities(ctx)
		for _, r := range rows {
			if r.UserID == "silent" {
				return r.Ghost
			}
		}
		t.Fatal("silent missing")
		return false
	}
	if err := s.RecordMessage(ctx, "silent", now, old, true); err != nil {
		t.Fatal(err)
	}
	if !flag() {
		t.Error("flag cleared while the role is still held")
	}
	if err := s.RecordMessage(ctx, "silent", now, old, false); err != nil {
		t.Fatal(err)
	}
	if flag() {
		t.Error("flag kept after a message without the role")
	}

	// A message from an unknown member adds them; a rejoin restarts the clock.
	if err := s.RecordMessage(ctx, "stranger", now, now, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordJoin(ctx, "poster", now); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMemberActivity(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.MemberActivities(ctx)
	ids := map[string]MemberActivity{}
	for _, r := range rows {
		ids[r.UserID] = r
	}
	if _, ok := ids["stranger"]; !ok || ids["new"].UserID != "" {
		t.Errorf("rows after events = %+v", ids)
	}
	if p := ids["poster"]; p.LastMessageAt == nil || p.JoinedAt.Before(now.Add(-time.Second)) {
		t.Errorf("rejoin = %+v, want last message kept and a fresh join time", p)
	}
}
