package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStudyNotes(t *testing.T) {
	s := openTestLibrary(t)
	ctx := context.Background()

	for _, d := range []string{`{"d":1}`, `{"d":1,"t":"Κεφ. 2"}`} { // the latest save wins
		if err := s.SetStudyNote(ctx, "u1", "f1", "study", d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetStudyNote(ctx, "u1", "f1", "ink", `{"1":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStudyNote(ctx, "u2", "f1", "study", `{"d":1}`); err != nil {
		t.Fatal(err)
	}
	got, err := s.StudyNotes(ctx, "u1")
	if err != nil || len(got) != 2 {
		t.Fatalf("notes = %+v, %v", got, err)
	}
	for _, n := range got {
		if n.Kind == "study" && n.Data != `{"d":1,"t":"Κεφ. 2"}` {
			t.Errorf("study = %s", n.Data)
		}
	}
	if err := s.SetStudyNote(ctx, "u1", "f1", "ink", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StudyNotes(ctx, "u1"); len(got) != 1 || got[0].Kind != "study" {
		t.Errorf("after delete = %+v", got)
	}
}

func TestStudyNotesQuota(t *testing.T) {
	s := openTestLibrary(t)
	ctx := context.Background()

	// A note that fits is stored, and replacing it with another of the same
	// size is always allowed: what a save replaces does not count.
	big := `{"d":"` + strings.Repeat("x", 1<<20) + `"}`
	saved := 0
	for i := 0; ; i++ {
		err := s.SetStudyNote(ctx, "u1", fmt.Sprint("f", i), "ink", big)
		if errors.Is(err, ErrStudyQuota) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if saved++; saved > 64 {
			t.Fatal("no quota after 64 MB of notes")
		}
	}
	if want := StudyNotesQuota / len(big); saved != want {
		t.Errorf("stored %d notes of %d bytes before the quota, want %d", saved, len(big), want)
	}
	if err := s.SetStudyNote(ctx, "u1", "f0", "ink", big); err != nil {
		t.Errorf("replacing a note at the quota: %v", err)
	}
	if err := s.SetStudyNote(ctx, "u1", "f0", "ink", ""); err != nil {
		t.Errorf("deleting at the quota: %v", err)
	}
	if err := s.SetStudyNote(ctx, "u1", "fnew", "study", `{"d":1}`); err != nil {
		t.Errorf("saving after freeing room: %v", err)
	}
	// The quota is per member.
	if err := s.SetStudyNote(ctx, "u2", "f0", "ink", big); err != nil {
		t.Errorf("another member's save: %v", err)
	}
}
