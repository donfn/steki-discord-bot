package bot

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestJobRunsOnceAtATime(t *testing.T) {
	var j Jobs
	end, ok := j.Begin("reconcile")
	if !ok {
		t.Fatal("the first start was refused")
	}
	if _, ok := j.Begin("reconcile"); ok {
		t.Error("a job already running started a second time")
	}
	if endOther, ok := j.Begin("stickies"); !ok {
		t.Error("a different job was refused")
	} else {
		endOther()
	}

	end()
	end2, ok := j.Begin("reconcile")
	if !ok {
		t.Fatal("the job could not start again after it ended")
	}
	end2()
	if len(j.running) != 0 {
		t.Errorf("%d jobs left in the table after all ended", len(j.running))
	}
}

func TestJobAdmitsExactlyOneUnderContention(t *testing.T) {
	var (
		j        Jobs
		admitted atomic.Int32
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := j.Begin("scan"); ok {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := admitted.Load(); n != 1 {
		t.Errorf("%d callers were admitted at once, want 1", n)
	}
}
