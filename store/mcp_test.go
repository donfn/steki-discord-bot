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
