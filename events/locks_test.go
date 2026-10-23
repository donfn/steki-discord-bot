package events

import (
	"io"
	"log/slog"
	"sync"
	"testing"
)

func TestKeyedMutexSerialisesSameKey(t *testing.T) {
	var k keyedMutex
	var wg sync.WaitGroup
	counter := 0

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := k.Lock("user")
			defer unlock()
			v := counter
			counter = v + 1 // a lost update here means two holders overlapped
		}()
	}
	wg.Wait()

	if counter != 50 {
		t.Fatalf("counter = %d, want 50", counter)
	}
	if len(k.locks) != 0 {
		t.Fatalf("%d lock entries leaked", len(k.locks))
	}
}

func TestKeyedMutexIndependentKeys(t *testing.T) {
	var k keyedMutex
	unlockA := k.Lock("a")
	defer unlockA()

	// Holding "a" must not block "b".
	done := make(chan struct{})
	go func() {
		k.Lock("b")()
		close(done)
	}()
	<-done
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
