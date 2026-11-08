package s3db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// memObjects is a bucket in memory that can be told to fail.
type memObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	etags   map[string]string
	n       int
	// fail, if set, decides whether an operation fails.
	fail func(op, key string) error
	// lostAnswer makes the next manifest write happen but report failure.
	lostAnswer bool
}

func newMem() *memObjects {
	return &memObjects{objects: map[string][]byte{}, etags: map[string]string{}}
}

func (m *memObjects) check(op, key string) error {
	if m.fail != nil {
		return m.fail(op, key)
	}
	return nil
}

func (m *memObjects) setFail(f func(op, key string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = f
}

func (m *memObjects) Get(_ context.Context, key string, w io.Writer) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("get", key); err != nil {
		return "", err
	}
	b, ok := m.objects[key]
	if !ok {
		return "", ErrNotFound
	}
	_, err := w.Write(b)
	return m.etags[key], err
}

func (m *memObjects) store(key string, b []byte) string {
	m.n++
	m.objects[key] = b
	m.etags[key] = strconv.Itoa(m.n)
	return m.etags[key]
}

func (m *memObjects) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("put", key); err != nil {
		return err
	}
	m.store(key, b)
	return nil
}

func (m *memObjects) PutIf(_ context.Context, key string, body []byte, etag string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("putif", key); err != nil {
		return "", err
	}
	cur, exists := m.etags[key]
	if (etag == "" && exists) || (etag != "" && cur != etag) {
		return "", ErrConflict
	}
	e := m.store(key, bytes.Clone(body))
	if m.lostAnswer {
		m.lostAnswer = false
		return "", errors.New("connection reset")
	}
	return e, nil
}

func (m *memObjects) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("delete", key); err != nil {
		return err
	}
	delete(m.objects, key)
	delete(m.etags, key)
	return nil
}

func (m *memObjects) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func (m *memObjects) keys(prefix string) int {
	k, _ := m.List(context.Background(), prefix)
	return len(k)
}

func openT(t *testing.T, o *memObjects, owner string, mod ...func(*Options)) *Shard {
	t.Helper()
	opts := Options{Name: "t", Dir: t.TempDir(), Objects: o, Owner: owner, Create: true}
	for _, f := range mod {
		f(&opts)
	}
	s, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// crash abandons the shard the way a killed container does: no final shipping,
// and the scratch disk is lost with it.
func crash(s *Shard) {
	close(s.stop)
	<-s.stopped
	_ = s.db.Close()
	_ = s.ctl.Close()
}

func exec(t *testing.T, s *Shard, q string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.DB().ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func count(t *testing.T, s *Shard) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`select count(*) from kv`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestVerifyAcceptsAConditionalBucket(t *testing.T) {
	o := newMem()
	if err := Verify(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if n := o.keys("db/"); n != 0 {
		t.Errorf("%d probe objects left behind", n)
	}
}

// ignoring is a bucket that accepts every conditional write.
type ignoring struct{ *memObjects }

func (i ignoring) PutIf(ctx context.Context, key string, body []byte, _ string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.store(key, body), nil
}

func TestVerifyRefusesABucketThatIgnoresConditions(t *testing.T) {
	if err := Verify(context.Background(), ignoring{newMem()}); err == nil {
		t.Fatal("a bucket ignoring If-Match passed")
	}
}

// Everything a statement was told is committed must survive a crash.
func TestAcknowledgedWritesSurviveACrash(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	exec(t, s, `create table kv (k integer primary key, v text)`)
	for i := range 50 {
		exec(t, s, `insert into kv values (?1, ?2)`, i, strings.Repeat("x", 500))
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`insert into kv values (1000, 'in a tx')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := s.DB().QueryRow(`insert into kv values (1001, 'r') returning k`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	crash(s)

	s2 := openT(t, o, "a")
	defer s2.Close()
	if n := count(t, s2); n != 52 {
		t.Fatalf("restored %d rows, want 52", n)
	}
}

func TestCompactionThenRestore(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	exec(t, s, `create table kv (k integer primary key, v text)`)
	for i := range maxSegments + 20 {
		exec(t, s, `insert into kv values (?1, 'v')`, i)
	}
	if n := o.keys("db/t/seg/"); n >= maxSegments {
		t.Errorf("%d segments left, compaction did not collect them", n)
	}
	if n := o.keys("db/t/daily/"); n != 1 {
		t.Errorf("%d daily images, want 1", n)
	}
	exec(t, s, `insert into kv values (9999, 'after')`)
	crash(s)
	s2 := openT(t, o, "a")
	defer s2.Close()
	if n := count(t, s2); n != maxSegments+21 {
		t.Fatalf("restored %d rows, want %d", n, maxSegments+21)
	}
}

// A commit that lands between the last segment and the checkpoint is only in
// the main file, and an upload failure right then must not let later segments
// skip over it.
func TestRestartedLogForcesAnImage(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	exec(t, s, `create table kv (k integer primary key, v text)`)
	exec(t, s, `insert into kv values (1, 'shipped')`)

	// Commit without shipping, by going past the barrier, and then checkpoint.
	raw := s.ctl
	if _, err := raw.Exec(`insert into kv values (2, 'unshipped')`); err != nil {
		t.Fatal(err)
	}
	var a, b, c int
	if err := raw.QueryRow(`pragma wal_checkpoint(truncate)`).Scan(&a, &b, &c); err != nil || a != 0 {
		t.Fatal(a, err)
	}
	if _, err := raw.Exec(`insert into kv values (3, 'after the restart')`); err != nil {
		t.Fatal(err)
	}
	// The next statement's barrier finds the log restarted.
	exec(t, s, `insert into kv values (4, 'next')`)
	crash(s)

	s2 := openT(t, o, "a")
	defer s2.Close()
	if n := count(t, s2); n != 4 {
		t.Fatalf("restored %d rows, want 4", n)
	}
}

func TestNewerWriterFencesTheOldOne(t *testing.T) {
	o := newMem()
	fenced := make(chan struct{})
	old := openT(t, o, "old", func(op *Options) { op.OnFenced = func() { close(fenced) } })
	exec(t, old, `create table kv (k integer primary key, v text)`)
	exec(t, old, `insert into kv values (1, 'a')`)

	young := openT(t, o, "new")
	defer young.Close()

	_, err := old.DB().Exec(`insert into kv values (2, 'lost')`)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("old writer got %v, want ErrFenced", err)
	}
	select {
	case <-fenced:
	case <-time.After(5 * time.Second):
		t.Fatal("OnFenced was not called")
	}
	if n := count(t, young); n != 1 {
		t.Fatalf("new writer has %d rows, want 1", n)
	}
	exec(t, young, `insert into kv values (2, 'kept')`)
	_ = old.Close()

	// The replaced deployment restarting must not take the shard back.
	_, err = Open(context.Background(), Options{Name: "t", Dir: t.TempDir(), Objects: o, Owner: "old"})
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("replaced deployment got %v, want ErrSuperseded", err)
	}
}

func TestSameDeploymentRestartsAndTakesBack(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	exec(t, s, `create table kv (k integer primary key, v text)`)
	crash(s)
	s2 := openT(t, o, "a")
	defer s2.Close()
	exec(t, s2, `insert into kv values (1, 'x')`)
}

func TestLostAnswerIsNotMistakenForFencing(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	defer s.Close()
	exec(t, s, `create table kv (k integer primary key, v text)`)
	o.mu.Lock()
	o.lostAnswer = true
	o.mu.Unlock()
	exec(t, s, `insert into kv values (1, 'x')`)
	exec(t, s, `insert into kv values (2, 'y')`)
	if n := count(t, s); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
}

// With the bucket down a write reports that it is not durable; reads keep
// working; and once the bucket is back the write arrives by itself.
func TestOutageDelaysButDoesNotLose(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	exec(t, s, `create table kv (k integer primary key, v text)`)

	o.setFail(func(_, _ string) error { return errors.New("bucket down") })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := s.DB().ExecContext(ctx, `insert into kv values (1, 'x')`)
	cancel()
	if !errors.Is(err, ErrNotDurable) {
		t.Fatalf("got %v, want ErrNotDurable", err)
	}
	if n := count(t, s); n != 1 {
		t.Fatalf("local read saw %d rows", n)
	}
	o.setFail(nil)

	deadline := time.Now().Add(40 * time.Second)
	for {
		m, _, err := readManifest(context.Background(), o, "t")
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Segments) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the write never reached the bucket")
		}
		time.Sleep(50 * time.Millisecond)
	}
	crash(s)
	s2 := openT(t, o, "a")
	defer s2.Close()
	if n := count(t, s2); n != 1 {
		t.Fatalf("restored %d rows, want 1", n)
	}
}

// A crash at any point in an upload, modelled as the bucket failing from the
// nth write on, must leave a database that opens and holds every acknowledged row.
func TestCrashAtEveryStep(t *testing.T) {
	for failAt := 1; failAt <= 12; failAt++ {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			o := newMem()
			s := openT(t, o, "a")
			exec(t, s, `create table kv (k integer primary key, v text)`)
			writes := 0
			o.setFail(func(op, _ string) error {
				if op == "put" || op == "putif" {
					if writes++; writes >= failAt {
						return errors.New("killed")
					}
				}
				return nil
			})
			acked := 0
			for i := range 6 {
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_, err := s.DB().ExecContext(ctx, `insert into kv values (?1, 'v')`, i)
				cancel()
				if err != nil {
					break
				}
				acked++
			}
			crash(s)
			o.setFail(nil)
			s2 := openT(t, o, "a")
			defer s2.Close()
			if n := count(t, s2); n < acked {
				t.Fatalf("restored %d rows, %d were acknowledged", n, acked)
			}
		})
	}
}

func TestDamagedObjectRefusesToOpen(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	exec(t, s, `create table kv (k integer primary key, v text)`)
	crash(s)
	m, _, err := readManifest(context.Background(), o, "t")
	if err != nil {
		t.Fatal(err)
	}
	o.objects[m.Segments[0].Key][0] ^= 0xff
	if _, err := Open(context.Background(), Options{Name: "t", Dir: t.TempDir(), Objects: o, Owner: "a"}); err == nil {
		t.Fatal("opened a shard with a damaged segment")
	}
}

func TestMissingShardIsNotCreatedSilently(t *testing.T) {
	_, err := Open(context.Background(), Options{Name: "t", Dir: t.TempDir(), Objects: newMem(), Owner: "a"})
	if !errors.Is(err, ErrNotSeeded) {
		t.Fatalf("got %v, want ErrNotSeeded", err)
	}
}

func TestSeedDoesNotOverwrite(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	exec(t, s, `create table kv (k integer primary key, v text)`)
	exec(t, s, `insert into kv values (1, 'real')`)
	crash(s)

	src := filepath.Join(t.TempDir(), "other.db")
	if err := os.WriteFile(src, []byte("not used"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := Seed(context.Background(), o, "t", src)
	if err != nil || ok {
		t.Fatalf("seed over a live shard: ok=%v err=%v", ok, err)
	}
	s2 := openT(t, o, "a")
	defer s2.Close()
	if n := count(t, s2); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

func TestLazyShardShipsOnClose(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a", func(op *Options) { op.Lazy = time.Hour })
	exec(t, s, `create table kv (k integer primary key, v text)`)
	exec(t, s, `insert into kv values (1, 'x')`)
	if n := o.keys("db/t/seg/"); n != 0 {
		t.Fatalf("lazy shard shipped %d segments early", n)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openT(t, o, "a")
	defer s2.Close()
	if n := count(t, s2); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

func TestConcurrentWritersShareUploads(t *testing.T) {
	o := newMem()
	s := openT(t, o, "a")
	defer s.Close()
	exec(t, s, `create table kv (k integer primary key, v text)`)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.DB().Exec(`insert into kv values (?1, 'v')`, i); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := count(t, s); n != 40 {
		t.Fatalf("%d rows, want 40", n)
	}
}
