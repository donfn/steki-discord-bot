package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func addTestMaterial(t *testing.T, s *Library, id, msg, home string, bot bool, posted time.Time) {
	t.Helper()
	ok, err := s.AddMaterial(context.Background(), &Material{
		AttachmentID: id, MessageID: msg, ChannelID: home, HomeChannelID: home, GuildID: "g",
		UploaderID: "u", UploaderBot: bot, Filename: id + ".pdf", ContentType: "application/pdf",
		Size: 10, SourceURL: "https://cdn/" + id, ObjectKey: "ch/" + id, PostedAt: posted,
	})
	if err != nil || !ok {
		t.Fatalf("add %s: ok=%v err=%v", id, ok, err)
	}
}

func TestMaterialLifecycle(t *testing.T) {
	s := openTestLibrary(t)
	ctx := context.Background()
	now := time.Now()
	addTestMaterial(t, s, "1", "m1", "c", false, now.Add(-3*time.Hour))
	addTestMaterial(t, s, "2", "m1", "c", false, now.Add(-2*time.Hour))
	addTestMaterial(t, s, "3", "m2", "c", true, now.Add(-time.Hour))

	if ok, err := s.AddMaterial(ctx, &Material{AttachmentID: "1", MessageID: "m1", ChannelID: "c",
		HomeChannelID: "c", GuildID: "g", UploaderID: "u", Filename: "x", ContentType: "application/pdf",
		SourceURL: "u", ObjectKey: "k", PostedAt: now}); err != nil || ok {
		t.Fatalf("duplicate add: ok=%v err=%v", ok, err)
	}

	// Nothing is listed before it is backed up and approved.
	if list, _ := s.ChannelMaterials(ctx, "c"); len(list) != 0 {
		t.Fatalf("listed %d before backup", len(list))
	}
	for _, id := range []string{"1", "2", "3"} {
		if err := s.MarkBackedUp(ctx, id, now); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := s.ChannelMaterials(ctx, "c"); len(list) != 0 {
		t.Fatalf("listed %d before screening", len(list))
	}
	pending, err := s.ScreeningBacklog(ctx, Cursor{}, 10, nil, false)
	if err != nil || len(pending) != 3 {
		t.Fatalf("screening backlog: %d, %v", len(pending), err)
	}
	for _, id := range []string{"1", "2", "3"} {
		if err := s.SetScreening(ctx, id, MaterialApproved, ""); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ChannelMaterials(ctx, "c")
	if err != nil || len(list) != 3 || list[0].AttachmentID != "3" {
		t.Fatalf("listing: %d files, first %v, err %v", len(list), list, err)
	}
	if n, _ := s.CountMemberMaterials(ctx, "c"); n != 2 {
		t.Errorf("member count %d, want 2 (bot files excluded)", n)
	}

	// An edit that drops attachment 2 hides only it.
	homes, err := s.DeleteMaterialsRemovedFrom(ctx, "m1", []string{"1"})
	if err != nil || len(homes) != 1 || homes[0] != "c" {
		t.Fatalf("prune: %v %v", homes, err)
	}
	if _, err := s.ChannelMaterial(ctx, "c", "2"); err != ErrNotFound {
		t.Errorf("pruned file still served: %v", err)
	}
	// Deleting the message hides the rest of it; deleting again is a no-op.
	if homes, _ := s.DeleteMaterialsByMessage(ctx, "m1", "nope"); len(homes) != 1 {
		t.Errorf("delete returned %v", homes)
	}
	if homes, _ := s.DeleteMaterialsByMessage(ctx, "m1"); len(homes) != 0 {
		t.Errorf("second delete returned %v", homes)
	}
	if n, _ := s.CountMemberMaterials(ctx, "c"); n != 0 {
		t.Errorf("member count after delete %d, want 0", n)
	}
	// A file is only served for its own home channel.
	if _, err := s.ChannelMaterial(ctx, "other", "3"); err != ErrNotFound {
		t.Errorf("served across channels: %v", err)
	}
	if m, err := s.ChannelMaterial(ctx, "c", "3"); err != nil || m.ObjectKey != "ch/3" {
		t.Errorf("visible file: %v %v", m, err)
	}
}

func TestMaterialBacklogPaging(t *testing.T) {
	s := openTestLibrary(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 7; i++ {
		addTestMaterial(t, s, fmt.Sprint(10+i), "m", "c", false, base.Add(time.Duration(i)*time.Second))
	}
	// Every row failing must not stop the walk from reaching later rows.
	var cur Cursor
	seen := 0
	for {
		rows, err := s.MaterialBacklog(ctx, 3, cur, 3, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rows {
			_ = s.MarkBackupFailed(ctx, m.AttachmentID, "boom")
			cur = cur.Next(m)
			seen++
		}
		if len(rows) < 3 {
			break
		}
	}
	if seen != 7 {
		t.Fatalf("walked %d rows, want 7", seen)
	}
	for i := 0; i < 2; i++ {
		for _, m := range mustBacklog(t, s) {
			_ = s.MarkBackupFailed(ctx, m.AttachmentID, "boom")
		}
	}
	if rows := mustBacklog(t, s); len(rows) != 0 {
		t.Errorf("%d rows still retried after max attempts", len(rows))
	}
	st, err := s.MaterialStats(ctx, 3)
	if err != nil || st.Total != 7 || st.Failed != 7 || st.BackedUp != 0 {
		t.Errorf("stats %+v %v", st, err)
	}
}

func mustBacklog(t *testing.T, s *Library) []*Material {
	t.Helper()
	rows, err := s.MaterialBacklog(context.Background(), 3, Cursor{}, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestScanStateAndSticky(t *testing.T) {
	s := openTestLibrary(t)
	ctx := context.Background()
	if st, err := s.MaterialScan(ctx, "c"); err != nil || st != (ScanState{}) {
		t.Fatalf("fresh scan state %+v %v", st, err)
	}
	want := ScanState{HighID: "9", LowID: "3", Done: true}
	if err := s.SetMaterialScan(ctx, "c", want); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.MaterialScan(ctx, "c"); st != want {
		t.Errorf("scan state %+v, want %+v", st, want)
	}
	if _, err := s.Sticky(ctx, "c"); err != ErrNotFound {
		t.Errorf("missing sticky: %v", err)
	}
	_ = s.SetSticky(ctx, "c", "a")
	_ = s.SetSticky(ctx, "c", "b")
	if id, _ := s.Sticky(ctx, "c"); id != "b" {
		t.Errorf("sticky %q, want b", id)
	}
}
