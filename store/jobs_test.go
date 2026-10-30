package store

import (
	"context"
	"testing"
)

func TestOneOffJobs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if done, err := s.JobDone(ctx, "a"); err != nil || done {
		t.Fatalf("fresh job done = %v, %v", done, err)
	}
	for range 2 {
		if err := s.MarkJobDone(ctx, "a"); err != nil {
			t.Fatal(err)
		}
	}
	if done, _ := s.JobDone(ctx, "a"); !done {
		t.Error("finished job not recorded")
	}
	if done, _ := s.JobDone(ctx, "b"); done {
		t.Error("another job counted as done")
	}
}
