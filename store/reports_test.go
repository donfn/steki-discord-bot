package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReportLimitAndDuplicates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	for i, id := range []string{"a", "b", "c", "d", "e"} {
		_, left, err := s.ReserveReport(ctx, id, "u1", "λόγος", now.Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatalf("report %s: %v", id, err)
		}
		if want := store5 - (i + 1); left != want {
			t.Fatalf("report %s: left %d, want %d", id, left, want)
		}
	}
	// The sixth is over the limit, and says when the first stops counting.
	_, _, err := s.ReserveReport(ctx, "f", "u1", "λόγος", now.Add(6*time.Hour))
	var lim *ReportLimitError
	if !errors.As(err, &lim) {
		t.Fatalf("sixth report: %v", err)
	}
	if want := now.Add(ReportWindow); lim.ResetsAt.Sub(want).Abs() > time.Second {
		t.Fatalf("resets at %v, want %v", lim.ResetsAt, want)
	}
	// Another member is unaffected.
	if _, _, err := s.ReserveReport(ctx, "f", "u2", "λόγος", now); err != nil {
		t.Fatalf("other member: %v", err)
	}
	// The same file twice is a duplicate, even over the limit.
	if _, _, err := s.ReserveReport(ctx, "a", "u1", "ξανά", now.Add(7*time.Hour)); !errors.Is(err, ErrReportDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	// A week later the first has aged out.
	if _, _, err := s.ReserveReport(ctx, "f", "u1", "λόγος", now.Add(ReportWindow+time.Minute)); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

const store5 = ReportsPerWeek

func TestDroppedReportGivesTheSlotBack(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	var ids []int64
	for _, f := range []string{"a", "b", "c", "d", "e"} {
		id, _, err := s.ReserveReport(ctx, f, "u1", "λόγος", now)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := s.DropReport(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReserveReport(ctx, "a", "u1", "λόγος", now); err != nil {
		t.Fatalf("after drop: %v", err)
	}
	// A settled report is not dropped.
	if err := s.SettleReport(ctx, ids[1], ReportInvalid, "όχι"); err != nil {
		t.Fatal(err)
	}
	if err := s.DropReport(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReserveReport(ctx, "z", "u1", "λόγος", now); err == nil {
		t.Fatal("a settled report still counts")
	}
}

func TestPullAndResolveReported(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	addTestMaterial(t, s, "1", "m1", "c", false, now)
	if err := s.SetScreening(ctx, "1", MaterialApproved, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBackedUp(ctx, "1", now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.PullReported(ctx, "other", "1", "x"); err != nil || ok {
		t.Fatalf("pulled through another channel: ok=%v err=%v", ok, err)
	}
	m, ok, err := s.PullReported(ctx, "c", "1", "δεν είναι υλικό")
	if err != nil || !ok || m.Status != MaterialReported {
		t.Fatalf("pull: ok=%v err=%v m=%+v", ok, err, m)
	}
	if list, _ := s.ChannelMaterials(ctx, "c"); len(list) != 0 {
		t.Fatal("a reported file is still listed")
	}
	if _, ok, _ := s.PullReported(ctx, "c", "1", "ξανά"); ok {
		t.Fatal("pulled twice")
	}

	// Putting it back keeps the bucket copy, so it is listed again at once.
	if _, won, err := s.ResolveReport(ctx, "1", false, "επαναφορά"); err != nil || !won {
		t.Fatalf("restore: won=%v err=%v", won, err)
	}
	if list, _ := s.ChannelMaterials(ctx, "c"); len(list) != 1 {
		t.Fatalf("restored file listed %d times", len(list))
	}
	// Taking it down for good clears the backup, and only one admin wins.
	if _, _, err := s.PullReported(ctx, "c", "1", "ξανά"); err != nil {
		t.Fatal(err)
	}
	m, won, err := s.ResolveReport(ctx, "1", true, "αφαίρεση")
	if err != nil || !won || m.Status != MaterialRejected || m.BackedUpAt != nil {
		t.Fatalf("remove: won=%v err=%v m=%+v", won, err, m)
	}
	if _, won, _ := s.ResolveReport(ctx, "1", false, "αργά"); won {
		t.Fatal("a decided report was decided again")
	}
}

func TestReportsToAnnounce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, _, _ := s.ReserveReport(ctx, "a", "u1", "λόγος", time.Now())
	if rows, _ := s.ReportsToAnnounce(ctx, 10); len(rows) != 0 {
		t.Fatal("a report still being checked is announced")
	}
	if err := s.SettleReport(ctx, id, ReportValid, "ναι"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.ReportsToAnnounce(ctx, 10)
	if len(rows) != 1 || rows[0].Note != "ναι" {
		t.Fatalf("to announce: %+v", rows)
	}
	if err := s.SetReportCard(ctx, id, "m"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.ReportsToAnnounce(ctx, 10); len(rows) != 0 {
		t.Fatal("announced twice")
	}
}
