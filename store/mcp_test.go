package store

import (
	"context"
	"testing"
	"time"
)

func TestMCPAnnouncements(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, a := range [][2]string{{"c1", "m1"}, {"c2", "m2"}, {"c1", "m1"}} { // the last is a repeat
		if err := s.AddMCPAnnouncement(ctx, a[0], a[1]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	got, err := s.MCPAnnouncements(ctx)
	if err != nil || len(got) != 2 || got[0].MessageID != "m1" || got[1].ChannelID != "c2" {
		t.Fatalf("announcements = %+v, %v", got, err)
	}
	if err := s.DeleteMCPAnnouncement(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.MCPAnnouncements(ctx); len(got) != 1 || got[0].MessageID != "m2" {
		t.Errorf("after delete = %+v", got)
	}
}

func TestMaterialReviewFlow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	addTestMaterial(t, s, "1", "m1", "c", false, now.Add(-3*time.Hour)) // new, not yet looked at
	addTestMaterial(t, s, "2", "m2", "c", false, now.Add(-2*time.Hour)) // listed before the check existed
	// Only images get the content check.
	if _, err := s.db.ExecContext(ctx, `update materials set content_type = 'image/jpeg'`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBackedUp(ctx, "2", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScreening(ctx, "2", MaterialApproved, ""); err != nil {
		t.Fatal(err)
	}

	ids := func(rows []*Material, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out string
		for _, m := range rows {
			out += m.AttachmentID
		}
		return out
	}
	// With the content check on, a new image is screened before its backup,
	// and the backup waits for the verdict.
	if got := ids(s.ScreeningBacklog(ctx, Cursor{}, 10, true, false)); got != "1" {
		t.Errorf("screening backlog = %q, want 1", got)
	}
	if got := ids(s.ScreeningBacklog(ctx, Cursor{}, 10, false, false)); got != "" {
		t.Errorf("without the check the unbacked image is not screened, got %q", got)
	}
	if got := ids(s.MaterialBacklog(ctx, 3, Cursor{}, 10, true)); got != "" {
		t.Errorf("backup backlog = %q, want none until approved", got)
	}
	// Rescan visits the listed image that never had the check.
	if got := ids(s.ScreeningBacklog(ctx, Cursor{}, 10, true, true)); got != "12" {
		t.Errorf("rescan backlog = %q, want 12", got)
	}
	if n, _ := s.CountUnchecked(ctx); n != 1 {
		t.Errorf("unchecked = %d, want 1", n)
	}
	if err := s.MarkVisionChecked(ctx, "2"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountUnchecked(ctx); n != 0 {
		t.Errorf("unchecked after check = %d", n)
	}

	// Flagging a listed image unlists it, clears its backup and queues a card.
	if err := s.HoldMaterial(ctx, "2", "meme"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ChannelMaterials(ctx, "c"); len(got) != 0 {
		t.Errorf("held image is still listed: %+v", got)
	}
	if got := ids(s.MaterialBacklog(ctx, 3, Cursor{}, 10, true)); got != "" {
		t.Errorf("held image is queued for backup: %q", got)
	}
	if got := ids(s.ReviewsToAnnounce(ctx, 10)); got != "2" {
		t.Errorf("reviews to announce = %q", got)
	}
	if err := s.SetReviewMessage(ctx, "2", "card"); err != nil {
		t.Fatal(err)
	}
	if got := ids(s.ReviewsToAnnounce(ctx, 10)); got != "" {
		t.Errorf("announced twice: %q", got)
	}

	// Approval goes back to the backup queue; the first decision wins.
	m, won, err := s.ResolveReview(ctx, "2", true, "ok")
	if err != nil || !won || m.Status != MaterialApproved || m.BackedUpAt != nil {
		t.Fatalf("approve: %+v won=%v err=%v", m, won, err)
	}
	if got := ids(s.MaterialBacklog(ctx, 3, Cursor{}, 10, true)); got != "2" {
		t.Errorf("approved image not queued for backup: %q", got)
	}
	if m, won, _ := s.ResolveReview(ctx, "2", false, "late"); won || m.Status != MaterialApproved {
		t.Errorf("second decision applied: %+v won=%v", m, won)
	}
}

func TestCourseMaterialStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	addTestMaterial(t, s, "1", "m1", "c", false, now.Add(-2*time.Hour))
	addTestMaterial(t, s, "2", "m2", "c", false, now.Add(-time.Hour))
	addTestMaterial(t, s, "3", "m3", "other", false, now)

	// Only approved, backed-up, undeleted files count.
	for _, id := range []string{"1", "2", "3"} {
		if err := s.MarkBackedUp(ctx, id, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"1", "2"} {
		if err := s.SetScreening(ctx, id, MaterialApproved, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CourseMaterialStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("stats = %+v, want only channel c (3 is not approved)", got)
	}
	if c := got["c"]; c.Files != 2 || c.Bytes != 20 || !c.Latest.Equal(now.Add(-time.Hour)) {
		t.Errorf("c = %+v", c)
	}
}
