package usernotes

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeBucket struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	failGet bool
	failPut bool
}

func (b *fakeBucket) Fetch(_ context.Context, key string, w io.Writer) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failGet {
		return false, errors.New("bucket down")
	}
	data, ok := b.objects[key]
	if !ok {
		return false, nil
	}
	_, err := w.Write(data)
	return true, err
}

func (b *fakeBucket) PutFile(_ context.Context, key, path, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failPut {
		return errors.New("bucket down")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	b.objects[key] = data
	b.puts++
	return nil
}

const user = "123456789012345678"

func setup(t *testing.T) (*Manager, *fakeBucket) {
	t.Helper()
	b := &fakeBucket{objects: map[string][]byte{}}
	m, err := New(b, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return m, b
}

func workingCopies(m *Manager) int {
	p, _ := filepath.Glob(filepath.Join(m.dir, "*.db"))
	return len(p)
}

func TestUploadsWhenLastSocketCloses(t *testing.T) {
	m, b := setup(t)
	ctx := context.Background()
	first, err := m.Open(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Open(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Save(ctx, user, "f1", "study", `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	first()
	first() // twice is once
	if b.puts != 0 {
		t.Fatal("uploaded with a socket still open")
	}
	second()
	if b.puts != 1 || workingCopies(m) != 0 {
		t.Fatalf("puts %d, working copies %d", b.puts, workingCopies(m))
	}

	// The next page load fetches it back.
	release, err := m.Open(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Notes(ctx, user)
	if err != nil || len(got) != 1 || got[0].Data != `{"a":1}` {
		t.Fatalf("%+v %v", got, err)
	}
	release()
	if b.puts != 1 {
		t.Errorf("uploaded an unchanged database: %d", b.puts)
	}
}

func TestNewMemberWithoutNotesUploadsNothing(t *testing.T) {
	m, b := setup(t)
	release, err := m.Open(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if b.puts != 0 || workingCopies(m) != 0 {
		t.Errorf("puts %d, working copies %d", b.puts, workingCopies(m))
	}
}

func TestPageLoadWithoutSocketEnds(t *testing.T) {
	m, _ := setup(t)
	if _, err := m.Notes(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	if workingCopies(m) != 1 {
		t.Fatal("not kept for the socket")
	}
	m.expire(user)
	if workingCopies(m) != 0 {
		t.Error("left open with nobody using it")
	}
}

func TestSocketOutlivesPageLease(t *testing.T) {
	m, b := setup(t)
	ctx := context.Background()
	if _, err := m.Notes(ctx, user); err != nil {
		t.Fatal(err)
	}
	release, err := m.Open(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Save(ctx, user, "f1", "ink", `{"1":[]}`); err != nil {
		t.Fatal(err)
	}
	release()
	if b.puts != 1 {
		t.Errorf("the page's lease held back the upload: %d", b.puts)
	}
}

func TestFailedFetchNeverStartsEmpty(t *testing.T) {
	m, b := setup(t)
	b.failGet = true
	if _, err := m.Open(context.Background(), user); err == nil {
		t.Fatal("opened without the member's database")
	}
	if workingCopies(m) != 0 {
		t.Error("left an empty database that would overwrite theirs")
	}
}

func TestFailedUploadIsKeptAndRetried(t *testing.T) {
	m, b := setup(t)
	ctx := context.Background()
	release, _ := m.Open(ctx, user)
	if err := m.Save(ctx, user, "f1", "study", `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	b.failPut = true
	release()
	if workingCopies(m) != 1 {
		t.Fatal("working copy lost")
	}

	// The member comes back before the retry: the working copy wins.
	release, err := m.Open(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Notes(ctx, user); len(got) != 1 {
		t.Fatalf("notes lost: %+v", got)
	}
	release()

	b.failPut = false
	m.sweep(ctx)
	if b.puts != 1 || workingCopies(m) != 0 {
		t.Errorf("puts %d, working copies %d", b.puts, workingCopies(m))
	}
}

func TestRestartUploadsWhatWasLeft(t *testing.T) {
	m, b := setup(t)
	ctx := context.Background()
	if _, err := m.Open(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(ctx, user, "f1", "study", `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	// The process dies here, sockets and all.
	m.entries[user].db.Close()

	next, err := New(b, m.dir, m.log)
	if err != nil {
		t.Fatal(err)
	}
	next.sweep(ctx)
	if b.puts != 1 || workingCopies(next) != 0 {
		t.Fatalf("puts %d, working copies %d", b.puts, workingCopies(next))
	}
	if !bytes.HasPrefix(b.objects[objectKey(user)], []byte("SQLite format 3")) {
		t.Error("object is not a database")
	}
}

func TestQuotaAndDelete(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	release, _ := m.Open(ctx, user)
	defer release()
	big := `{"d":"` + strings.Repeat("x", 1<<20) + `"}`
	var err error
	saved := 0
	for i := 0; i < 40 && err == nil; i++ {
		if err = m.Save(ctx, user, "f"+string(rune('A'+i)), "ink", big); err == nil {
			saved++
		}
	}
	if !errors.Is(err, ErrQuota) || saved != Quota/len(big) {
		t.Fatalf("saved %d, %v", saved, err)
	}
	if err := m.Save(ctx, user, "fA", "ink", big); err != nil {
		t.Errorf("replacing counted against the quota: %v", err)
	}
	if err := m.Save(ctx, user, "fA", "ink", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(ctx, user, "new", "ink", big); err != nil {
		t.Errorf("after a delete: %v", err)
	}
}

func TestBadUserRefused(t *testing.T) {
	m, _ := setup(t)
	if _, err := m.Open(context.Background(), "../../etc/x"); err == nil {
		t.Error("accepted a path as a user")
	}
}
