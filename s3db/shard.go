// Package s3db keeps SQLite databases in an S3 bucket, with no volume.
//
// Each shard is an ordinary SQLite file on the container's scratch disk with
// exactly one writing process. The bucket holds the truth:
//
//	db/<shard>/manifest.json     the commit point: which objects make the database
//	db/<shard>/snap/…            whole database images, plain SQLite files
//	db/<shard>/seg/…             the pages changed since that image
//	db/<shard>/daily/…           one image a day, kept for two weeks
//
// Starting up downloads the image and writes the segments' pages over it.
// Running, SQLite commits to its write-ahead log as usual, and a single goroutine
// reads the committed frames out of the log, uploads them as a segment, and adds
// the segment to the manifest with a conditional write. A statement's result is
// held back until that has happened, through the driver in driver.go, so what a
// caller was told is committed survives the container. Statements waiting at the
// same time share one upload, which is what makes bursts cheap.
//
// Fencing: every writer takes the manifest over by raising its epoch, and every
// later manifest write is conditional on the etag it last saw. An older writer's
// next write is refused and it stops, so a new deployment overlapping the old
// one can never fork the database.
//
// Automatic checkpoints are off, so the log only restarts when this package says
// so, and the main file changes only then. That makes the main file a stable
// image to upload while writers carry on into the log.
//
// Nothing here is compressed. The databases are small, and an image can be opened
// as it is with any SQLite tool.
package s3db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	// commitTimeout bounds how long a commit with no context of its own waits
	// for the bucket.
	commitTimeout = 20 * time.Second
	// maxSegments and minCompactBytes decide when segments fold into a new image.
	maxSegments     = 128
	minCompactBytes = 1 << 20
	compactEvery    = 24 * time.Hour
	keepHistory     = 3
	keepDaily       = 14
	keepRetired     = 16
	retryMin        = time.Second
	retryMax        = 30 * time.Second
)

var (
	// ErrFenced means a newer writer took the shard over.
	ErrFenced = errors.New("s3db: another writer took this shard over")
	// ErrSuperseded means this deployment was already replaced, so it must not
	// take the shard back from its successor.
	ErrSuperseded = errors.New("s3db: this deployment has been replaced")
	// ErrNotSeeded means the shard does not exist in the bucket. It is never
	// created implicitly: an empty registry would let members reuse addresses.
	ErrNotSeeded = errors.New("s3db: shard does not exist in the bucket")
	// ErrNotDurable means the commit happened locally but did not reach the
	// bucket in time. Shipping keeps retrying, so it may still arrive.
	ErrNotDurable = errors.New("s3db: commit not yet durable")
)

// objectRef names an object and what it must hash to.
type objectRef struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// manifest is the shard's commit point.
type manifest struct {
	Format     int         `json:"format"`
	Shard      string      `json:"shard"`
	Epoch      uint64      `json:"epoch"`
	Owner      string      `json:"owner"`
	Retired    []string    `json:"retired,omitempty"`
	Seq        uint64      `json:"seq"`
	Snapshot   objectRef   `json:"snapshot"`
	SnapshotAt time.Time   `json:"snapshot_at"`
	Segments   []objectRef `json:"segments,omitempty"`
	History    []objectRef `json:"history,omitempty"`
}

func manifestKey(shard string) string { return "db/" + shard + "/manifest.json" }

// Options configures a shard.
type Options struct {
	Name    string
	Dir     string // scratch directory; its contents are replaced on Open
	Objects Objects
	Log     *slog.Logger
	// Owner identifies this deployment (RAILWAY_DEPLOYMENT_ID). A restart of the
	// same deployment takes the shard back; one that was replaced does not.
	Owner string
	// Lazy, when above zero, stops statements from waiting for the bucket and
	// ships at this interval instead, for data whose last moments can be lost.
	Lazy time.Duration
	// Create makes an empty shard if none exists.
	Create bool
	// OnFenced is called once when a newer writer takes over.
	OnFenced func()
}

// Shard is one open database.
type Shard struct {
	o    Options
	path string
	db   *sql.DB
	ctl  *sql.DB // keeps the log alive, and runs checkpoints

	// Touched only by the shipping goroutine, and by Open and Close around it.
	m            manifest
	etag         string
	cur          walCursor
	needSnapshot bool

	seenSize atomic.Int64
	kick     chan struct{}
	stop     chan struct{}
	stopped  chan struct{}

	mu      sync.Mutex
	started uint64
	done    uint64
	lastErr error
	fenced  error
	changed chan struct{} // closed and replaced when the fields above change
}

// Seed publishes the SQLite file at src as a new shard. It reports false when
// the shard already exists, in which case nothing changes. The file must have no
// write-ahead log of its own.
func Seed(ctx context.Context, o Objects, name, src string) (bool, error) {
	if _, err := os.Stat(src + "-wal"); err == nil {
		return false, fmt.Errorf("s3db: seed %s: %s has a write-ahead log", name, src)
	}
	ref, err := uploadSnapshot(ctx, o, name, src, "seed")
	if err != nil {
		return false, err
	}
	body, err := json.Marshal(manifest{Format: 1, Shard: name, Snapshot: ref, SnapshotAt: time.Now().UTC()})
	if err != nil {
		return false, err
	}
	if _, err := o.PutIf(ctx, manifestKey(name), body, ""); err != nil {
		if errors.Is(err, ErrConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Exists reports whether the shard is in the bucket.
func Exists(ctx context.Context, o Objects, name string) (bool, error) {
	_, _, err := readManifest(ctx, o, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Open takes the shard over, rebuilds its file and starts shipping.
func Open(ctx context.Context, o Options) (*Shard, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Shard{
		o:       o,
		path:    filepath.Join(o.Dir, o.Name+".db"),
		kick:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		changed: make(chan struct{}),
	}
	if err := os.RemoveAll(o.Dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Dir, 0o750); err != nil {
		return nil, err
	}
	if err := s.takeOver(ctx); err != nil {
		return nil, err
	}
	if err := s.restore(ctx); err != nil {
		return nil, fmt.Errorf("s3db: restore %s: %w", o.Name, err)
	}
	if err := s.openDB(ctx); err != nil {
		return nil, err
	}
	go s.run()
	o.Log.Info("shard open", "shard", o.Name, "epoch", s.m.Epoch, "seq", s.m.Seq, "segments", len(s.m.Segments))
	return s, nil
}

// DB is the database handle. Statements through it wait for the bucket.
func (s *Shard) DB() *sql.DB { return s.db }

// Path is the working file on the scratch disk.
func (s *Shard) Path() string { return s.path }

func readManifest(ctx context.Context, o Objects, name string) (manifest, string, error) {
	var buf bytes.Buffer
	etag, err := o.Get(ctx, manifestKey(name), &buf)
	if err != nil {
		return manifest{}, "", err
	}
	var m manifest
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		return manifest{}, "", fmt.Errorf("s3db: manifest of %s: %w", name, err)
	}
	return m, etag, nil
}

// takeOver raises the epoch. Everything read afterwards is stable, since any
// earlier writer's next manifest write now fails.
func (s *Shard) takeOver(ctx context.Context) error {
	for {
		m, etag, err := readManifest(ctx, s.o.Objects, s.o.Name)
		if errors.Is(err, ErrNotFound) {
			if !s.o.Create {
				return fmt.Errorf("%w: %s", ErrNotSeeded, s.o.Name)
			}
			if err := s.createEmpty(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if slices.Contains(m.Retired, s.o.Owner) {
			return fmt.Errorf("%w: %s", ErrSuperseded, s.o.Owner)
		}
		if m.Owner != "" && m.Owner != s.o.Owner {
			m.Retired = append(m.Retired, m.Owner)
			if len(m.Retired) > keepRetired {
				m.Retired = m.Retired[len(m.Retired)-keepRetired:]
			}
		}
		m.Epoch++
		m.Owner = s.o.Owner
		body, err := json.Marshal(m)
		if err != nil {
			return err
		}
		newEtag, err := s.o.Objects.PutIf(ctx, manifestKey(s.o.Name), body, etag)
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		s.m, s.etag = m, newEtag
		return nil
	}
}

func (s *Shard) createEmpty(ctx context.Context) error {
	tmp := s.path + ".new"
	db, err := sql.Open("sqlite", "file:"+tmp+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `vacuum`)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, err = Seed(ctx, s.o.Objects, s.o.Name, tmp)
	return err
}

// restore builds the file from the image and the segments. Any object that is
// missing or does not hash to what the manifest says is an error: starting from
// less than the whole database would quietly forget data.
func (s *Shard) restore(ctx context.Context) error {
	tmp := s.path + ".restore"
	if err := downloadSnapshot(ctx, s.o.Objects, s.m.Snapshot, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, ref := range s.m.Segments {
		body, err := fetch(ctx, s.o.Objects, ref)
		if err != nil {
			return err
		}
		if err := applySegment(f, body); err != nil {
			return fmt.Errorf("segment %s: %w", ref.Key, err)
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func fetch(ctx context.Context, o Objects, ref objectRef) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := o.Get(ctx, ref.Key, &buf); err != nil {
		return nil, fmt.Errorf("%s: %w", ref.Key, err)
	}
	sum := sha256.Sum256(buf.Bytes())
	if hex.EncodeToString(sum[:]) != ref.SHA256 {
		return nil, fmt.Errorf("%s: contents do not match the manifest", ref.Key)
	}
	return buf.Bytes(), nil
}

func downloadSnapshot(ctx context.Context, o Objects, ref objectRef, dst string) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := o.Get(ctx, ref.Key, io.MultiWriter(f, hash)); err != nil {
		return fmt.Errorf("%s: %w", ref.Key, err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return fmt.Errorf("%s: contents do not match the manifest", ref.Key)
	}
	return f.Close()
}

// uploadSnapshot uploads the file at path as an image.
func uploadSnapshot(ctx context.Context, o Objects, shard, path, tag string) (objectRef, error) {
	f, err := os.Open(path)
	if err != nil {
		return objectRef{}, err
	}
	defer f.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	if err != nil {
		return objectRef{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return objectRef{}, err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	ref := objectRef{Key: fmt.Sprintf("db/%s/snap/%s-%s.db", shard, tag, sum[:16]), SHA256: sum, Size: size}
	if err := o.Put(ctx, ref.Key, f, size); err != nil {
		return objectRef{}, err
	}
	return ref, nil
}

func (s *Shard) dsn(pragmas ...string) string {
	return "file:" + s.path + "?" + url.Values{"_pragma": pragmas, "_txlock": {"immediate"}}.Encode()
}

func (s *Shard) openDB(ctx context.Context) error {
	var err error
	if s.ctl, err = sql.Open("sqlite", s.dsn("busy_timeout(1000)", "journal_mode(WAL)", "wal_autocheckpoint(0)")); err != nil {
		return err
	}
	s.ctl.SetMaxOpenConns(1)
	s.ctl.SetMaxIdleConns(1)
	s.ctl.SetConnMaxLifetime(0)
	var check string
	if err := s.ctl.QueryRowContext(ctx, `pragma quick_check`).Scan(&check); err != nil {
		_ = s.ctl.Close()
		return fmt.Errorf("s3db: check %s: %w", s.o.Name, err)
	}
	if check != "ok" {
		_ = s.ctl.Close()
		return fmt.Errorf("s3db: %s restored damaged: %s", s.o.Name, check)
	}
	s.db = sql.OpenDB(&connector{s: s, dsn: s.dsn(
		"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(1)",
		"cache_size(-1024)", "mmap_size(0)", "wal_autocheckpoint(0)")})
	// Writers are serialised by SQLite anyway; a second connection lets reads
	// proceed beside one.
	s.db.SetMaxOpenConns(2)
	s.db.SetMaxIdleConns(2)
	s.db.SetConnMaxLifetime(0)
	return nil
}

// barrier waits until everything committed before the call is in the bucket.
func (s *Shard) barrier(ctx context.Context) error {
	if s.o.Lazy > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.fenced
	}
	if !s.walChanged() {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.fenced
	}
	s.mu.Lock()
	want := s.started + 1
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	for {
		s.mu.Lock()
		done, lastErr, fenced, changed := s.done, s.lastErr, s.fenced, s.changed
		s.mu.Unlock()
		if fenced != nil {
			return fenced
		}
		if done >= want {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w: %w", ErrNotDurable, lastErr)
			}
			return fmt.Errorf("%w: %w", ErrNotDurable, ctx.Err())
		}
	}
}

// walChanged is the barrier's fast path, which is what a read pays: it is true
// only if the log has changed since shipping last looked at it.
func (s *Shard) walChanged() bool {
	fi, err := os.Stat(s.path + "-wal")
	if err != nil {
		return s.seenSize.Load() != 0
	}
	return fi.Size() != s.seenSize.Load()
}

func (s *Shard) run() {
	defer close(s.stopped)
	interval := time.Minute
	if s.o.Lazy > 0 {
		interval = s.o.Lazy
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	backoff := retryMin
	var retry <-chan time.Time
	for {
		select {
		case <-s.stop:
			return
		case <-s.kick:
		case <-tick.C:
		case <-retry:
		}
		retry = nil
		if s.passOnce() {
			backoff = retryMin
			continue
		}
		if s.isFenced() {
			<-s.stop
			return
		}
		retry = time.After(backoff)
		backoff = min(backoff*2, retryMax)
	}
}

func (s *Shard) isFenced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fenced != nil
}

// passOnce runs one shipping pass and wakes whoever it satisfied.
func (s *Shard) passOnce() bool {
	s.mu.Lock()
	s.started++
	gen := s.started
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := s.pass(ctx)
	cancel()

	fenced := errors.Is(err, ErrFenced)
	s.mu.Lock()
	if err == nil {
		s.done, s.lastErr = gen, nil
	} else {
		s.lastErr = err
	}
	first := fenced && s.fenced == nil
	if fenced {
		s.fenced = err
	}
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()

	if err != nil && !fenced {
		s.o.Log.Error("shard: shipping failed", "shard", s.o.Name, "error", err)
	}
	if first {
		s.o.Log.Warn("shard: a newer writer took over", "shard", s.o.Name)
		if s.o.OnFenced != nil {
			go s.o.OnFenced()
		}
	}
	return err == nil
}

// pass ships what is new, then folds the segments into an image if it is time.
func (s *Shard) pass(ctx context.Context) error {
	if s.needSnapshot {
		if err := s.publishSnapshot(ctx); err != nil {
			return err
		}
	}
	var size int64
	if fi, err := os.Stat(s.path + "-wal"); err == nil {
		size = fi.Size()
	}
	batch, next, err := readWAL(s.path+"-wal", s.cur)
	if errors.Is(err, errWALRestarted) {
		// Frames that were not shipped are in the main file now, so only a new
		// image can cover them. Segments must not continue until one exists.
		s.cur, s.needSnapshot = walCursor{}, true
		if err := s.publishSnapshot(ctx); err != nil {
			return err
		}
		batch, next, err = readWAL(s.path+"-wal", s.cur)
	}
	if err != nil {
		return err
	}
	if batch != nil {
		if err := s.shipSegment(ctx, batch); err != nil {
			return err
		}
		s.cur = next
	}
	s.seenSize.Store(size)
	if s.compactDue() {
		return s.compact(ctx)
	}
	return nil
}

func (s *Shard) shipSegment(ctx context.Context, batch *walBatch) error {
	body := encodeSegment(batch)
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	seq := s.m.Seq + 1
	ref := objectRef{
		Key:    fmt.Sprintf("db/%s/seg/%016x-%016x-%s", s.o.Name, seq, s.m.Epoch, hash[:16]),
		SHA256: hash, Size: int64(len(body)),
	}
	if err := s.o.Objects.Put(ctx, ref.Key, bytes.NewReader(body), ref.Size); err != nil {
		return fmt.Errorf("upload segment: %w", err)
	}
	next := s.m
	next.Seq = seq
	next.Segments = append(slices.Clone(s.m.Segments), ref)
	return s.commit(ctx, next)
}

// commit writes the manifest. A write whose answer was lost may have happened
// (the client also retries by itself, and then sees its own write as a
// conflict), so any failure is settled by reading what is there.
func (s *Shard) commit(ctx context.Context, next manifest) error {
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	etag, err := s.o.Objects.PutIf(ctx, manifestKey(s.o.Name), body, s.etag)
	if err == nil {
		s.m, s.etag = next, etag
		return nil
	}
	cur, curEtag, rerr := readManifest(ctx, s.o.Objects, s.o.Name)
	if rerr != nil {
		return fmt.Errorf("write manifest: %w (and read it back: %v)", err, rerr)
	}
	if cur.Epoch != s.m.Epoch || cur.Owner != s.m.Owner {
		return fmt.Errorf("%w: epoch %d, ours %d", ErrFenced, cur.Epoch, s.m.Epoch)
	}
	if curEtag == s.etag {
		return fmt.Errorf("write manifest: %w", err) // it did not happen; retry
	}
	// Our own earlier attempt landed. Adopt it. Segments replay in order and
	// hold whole pages, so shipping an overlapping one again is harmless.
	s.m, s.etag = cur, curEtag
	if reflectEqual(cur, next) {
		return nil
	}
	return fmt.Errorf("write manifest: an earlier attempt landed first: %w", err)
}

func reflectEqual(a, b manifest) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func (s *Shard) compactDue() bool {
	if len(s.m.Segments) == 0 {
		return false
	}
	var total int64
	for _, r := range s.m.Segments {
		total += r.Size
	}
	return len(s.m.Segments) >= maxSegments ||
		total > max(minCompactBytes, s.m.Snapshot.Size) ||
		time.Since(s.m.SnapshotAt) > compactEvery
}

// compact moves the log into the main file and publishes that as an image.
// A commit landing between the last segment and the checkpoint goes into the
// main file unshipped, so from the checkpoint on no segment may be written until
// an image exists; needSnapshot holds that across failed uploads. Its statement
// is still waiting, for a pass that has not finished, so it has been told nothing.
func (s *Shard) compact(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.ctl.QueryRowContext(ctx, `pragma wal_checkpoint(truncate)`).
		Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if busy != 0 {
		return nil // a reader is in the way; the next pass tries again
	}
	s.cur, s.needSnapshot = walCursor{}, true
	s.seenSize.Store(0)
	return s.publishSnapshot(ctx)
}

func (s *Shard) publishSnapshot(ctx context.Context) error {
	ref, err := uploadSnapshot(ctx, s.o.Objects, s.o.Name, s.path,
		fmt.Sprintf("%016x-%016x", s.m.Seq+1, s.m.Epoch))
	if err != nil {
		return fmt.Errorf("upload image: %w", err)
	}

	next := s.m
	next.Seq++
	next.Segments = nil
	next.Snapshot = ref
	next.SnapshotAt = time.Now().UTC()
	if s.m.Snapshot.Key != "" && s.m.Snapshot.Key != ref.Key {
		next.History = append([]objectRef{s.m.Snapshot}, s.m.History...)
		if len(next.History) > keepHistory {
			next.History = next.History[:keepHistory]
		}
	}
	if err := s.commit(ctx, next); err != nil {
		return err
	}
	s.needSnapshot = false
	s.keepDailyImage(ctx)
	s.collectGarbage(ctx)
	return nil
}

// keepDailyImage stores the day's first image apart. Railway buckets have
// neither versioning nor backups, so this is what undoes a bad day. Failure only
// costs that day's copy.
func (s *Shard) keepDailyImage(ctx context.Context) {
	prefix := "db/" + s.o.Name + "/daily/"
	key := prefix + time.Now().UTC().Format("2006-01-02") + ".db"
	keys, err := s.o.Objects.List(ctx, prefix)
	if err != nil || slices.Contains(keys, key) {
		return
	}
	f, err := os.Open(s.path) // stable until the next checkpoint, which only this goroutine runs
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	if err := s.o.Objects.Put(ctx, key, f, fi.Size()); err != nil {
		s.o.Log.Warn("shard: daily image", "shard", s.o.Name, "error", err)
		return
	}
	slices.Sort(keys)
	for len(keys) >= keepDaily {
		_ = s.o.Objects.Delete(ctx, keys[0])
		keys = keys[1:]
	}
}

// collectGarbage removes objects no manifest entry points at. It runs only after
// a commit, and only this writer adds entries, so nothing it removes can become
// wanted. An upload a fenced writer left behind is removed the same way.
func (s *Shard) collectGarbage(ctx context.Context) {
	keep := map[string]bool{s.m.Snapshot.Key: true}
	for _, r := range s.m.History {
		keep[r.Key] = true
	}
	for _, r := range s.m.Segments {
		keep[r.Key] = true
	}
	for _, dir := range []string{"snap/", "seg/"} {
		keys, err := s.o.Objects.List(ctx, "db/"+s.o.Name+"/"+dir)
		if err != nil {
			s.o.Log.Warn("shard: list old objects", "shard", s.o.Name, "error", err)
			return
		}
		for _, k := range keys {
			if !keep[k] && !strings.HasSuffix(k, "/") {
				if err := s.o.Objects.Delete(ctx, k); err != nil {
					s.o.Log.Warn("shard: remove old object", "shard", s.o.Name, "error", err)
				}
			}
		}
	}
}

// FileSize is the database's size on the scratch disk, log included.
func (s *Shard) FileSize() int64 {
	var n int64
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// Close ships what is left and closes the database.
func (s *Shard) Close() error {
	close(s.stop)
	<-s.stopped
	var err error
	if !s.isFenced() {
		if !s.passOnce() {
			s.mu.Lock()
			err = s.lastErr
			s.mu.Unlock()
		}
	}
	_ = s.db.Close()
	_ = s.ctl.Close()
	return err
}
