package store

import (
	"context"
	"testing"
)

func TestStudyNotes(t *testing.T) {
	s := openTestStore(t)
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
