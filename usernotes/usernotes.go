// Package usernotes keeps what each member saves on the material page in a
// SQLite database of their own, stored as one object in the bucket.
//
// A database is fetched when the member's page loads and uploaded again when
// the last of their sockets closes. In between, the working copy lives on the
// volume: a save is acknowledged once it is committed there, so a restart or a
// failed upload loses nothing. A working copy with nobody using it is therefore
// always something still to upload, and sweep retries those.
package usernotes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	// Quota is the most one member's notes may weigh in all, in bytes.
	Quota = 32 << 20

	// pageGrace keeps a database open after a page load for its socket to
	// arrive. A page with no socket, such as a read-only view, ends with it.
	pageGrace = 2 * time.Minute

	bucketTimeout = 2 * time.Minute
	sweepEvery    = time.Minute
	contentType   = "application/vnd.sqlite3"
)

// ErrQuota is returned by Save when it would take the member past Quota.
var ErrQuota = errors.New("usernotes: quota reached")

// Bucket is where the databases are stored.
type Bucket interface {
	// Fetch writes the object at key to w and reports whether it exists.
	Fetch(ctx context.Context, key string, w io.Writer) (bool, error)
	// PutFile uploads the file at path to key as a whole.
	PutFile(ctx context.Context, key, path, contentType string) error
}

// Note is one member's saved state of one kind for one file.
type Note struct {
	File, Kind, Data string
}

// Legacy reads and clears the notes saved before they moved to the bucket. Each
// member's are imported the first time they have no database yet.
type Legacy interface {
	LegacyNotes(ctx context.Context, userID string) ([]Note, error)
	ClearLegacyNotes(ctx context.Context, userID string) error
}

// Manager opens, closes and uploads the members' databases.
type Manager struct {
	bucket Bucket
	legacy Legacy
	dir    string
	log    *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
}

// entry is one member's database. mu serialises everything done to it, so an
// upload and a fresh page load can never interleave.
type entry struct {
	user string
	pins int // guarded by Manager.mu: callers holding or waiting for mu

	mu      sync.Mutex
	db      *sql.DB
	dirty   bool
	sockets int
	lease   *time.Timer
}

// New returns a Manager keeping working copies in dir, on the volume.
func New(bucket Bucket, legacy Legacy, dir string, log *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	// A half-finished download is worth nothing; the bucket still has it.
	tmp, _ := filepath.Glob(filepath.Join(dir, "*.tmp*"))
	for _, p := range tmp {
		_ = os.Remove(p)
	}
	return &Manager{bucket: bucket, legacy: legacy, dir: dir, log: log, entries: map[string]*entry{}}, nil
}

// Run uploads what earlier failed or a restart left behind, until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		m.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) sweep(ctx context.Context) {
	paths, err := filepath.Glob(filepath.Join(m.dir, "*.db"))
	if err != nil {
		return
	}
	for _, p := range paths {
		if ctx.Err() != nil {
			return
		}
		e := m.enter(strings.TrimSuffix(filepath.Base(p), ".db"))
		if e.db == nil {
			if err := m.load(ctx, e); err != nil {
				m.log.Error("usernotes: reopen working copy", "user_id", e.user, "error", err)
			}
		}
		m.settle(ctx, e)
		m.leave(e)
	}
}

func (m *Manager) enter(user string) *entry {
	m.mu.Lock()
	e := m.entries[user]
	if e == nil {
		e = &entry{user: user}
		m.entries[user] = e
	}
	e.pins++
	m.mu.Unlock()
	e.mu.Lock()
	return e
}

func (m *Manager) leave(e *entry) {
	open := e.db != nil
	e.mu.Unlock()
	m.mu.Lock()
	if e.pins--; e.pins == 0 && !open {
		delete(m.entries, e.user)
	}
	m.mu.Unlock()
}

func validUser(user string) bool {
	if user == "" {
		return false
	}
	return strings.Trim(user, "0123456789") == ""
}

func (m *Manager) path(user string) string { return filepath.Join(m.dir, user+".db") }

func objectKey(user string) string { return "user-notes/" + user + ".sqlite" }

func openDB(ctx context.Context, path string) (*sql.DB, error) {
	q := url.Values{"_pragma": {"journal_mode(delete)", "busy_timeout(5000)", "cache_size(-256)"}, "_txlock": {"immediate"}}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var check string
	if err = db.QueryRowContext(ctx, `pragma quick_check`).Scan(&check); err == nil && check != "ok" {
		err = fmt.Errorf("quick_check: %s", check)
	}
	if err == nil {
		_, err = db.ExecContext(ctx, `create table if not exists notes (
			file text not null,
			kind text not null,
			data text not null,
			updated_at integer not null,
			primary key (file, kind)) without rowid`)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// load opens the member's database: the working copy if one was left, else the
// bucket's, else a new one holding their legacy notes. A failed fetch is an
// error and never an empty database, which would overwrite theirs on upload.
func (m *Manager) load(ctx context.Context, e *entry) error {
	if !validUser(e.user) {
		return fmt.Errorf("usernotes: bad user %q", e.user)
	}
	path := m.path(e.user)
	if _, err := os.Stat(path); err == nil {
		db, err := openDB(ctx, path)
		if err != nil {
			return fmt.Errorf("open working copy: %w", err)
		}
		e.db, e.dirty = db, true
		return nil
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	found, err := m.download(ctx, e.user, tmp)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	db, err := openDB(ctx, tmp)
	if err != nil {
		return fmt.Errorf("open fetched copy: %w", err)
	}
	imported := false
	if !found {
		imported, err = m.importLegacy(ctx, e.user, db)
	}
	if err == nil {
		err = db.Close()
	} else {
		_ = db.Close()
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if e.db, err = openDB(ctx, path); err != nil {
		return err
	}
	e.dirty = imported
	return nil
}

func (m *Manager) download(ctx context.Context, user, tmp string) (bool, error) {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, bucketTimeout)
	defer cancel()
	found, err := m.bucket.Fetch(ctx, objectKey(user), f)
	if err != nil {
		return false, err
	}
	return found, f.Close()
}

func (m *Manager) importLegacy(ctx context.Context, user string, db *sql.DB) (bool, error) {
	if m.legacy == nil {
		return false, nil
	}
	old, err := m.legacy.LegacyNotes(ctx, user)
	if err != nil || len(old) == 0 {
		return false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, n := range old {
		if _, err := tx.ExecContext(ctx, `insert or replace into notes (file, kind, data, updated_at)
			values (?1, ?2, ?3, ?4)`, n.File, n.Kind, n.Data, time.Now().UnixMilli()); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// settle closes and uploads the database if nothing is using it any more. A
// failed upload keeps the working copy for sweep to retry.
func (m *Manager) settle(ctx context.Context, e *entry) {
	if e.db == nil || e.sockets > 0 || e.lease != nil {
		return
	}
	err := e.db.Close()
	e.db = nil
	if err != nil {
		m.log.Error("usernotes: close", "user_id", e.user, "error", err)
		return
	}
	if e.dirty {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bucketTimeout)
		defer cancel()
		if err := m.bucket.PutFile(ctx, objectKey(e.user), m.path(e.user), contentType); err != nil {
			m.log.Error("usernotes: upload; will retry", "user_id", e.user, "error", err)
			return
		}
		if m.legacy != nil {
			if err := m.legacy.ClearLegacyNotes(ctx, e.user); err != nil {
				m.log.Warn("usernotes: clear legacy notes", "user_id", e.user, "error", err)
			}
		}
		m.log.Debug("usernotes: uploaded", "user_id", e.user)
	}
	e.dirty = false
	if err := os.Remove(m.path(e.user)); err != nil {
		m.log.Warn("usernotes: remove working copy", "user_id", e.user, "error", err)
	}
}

// Open holds the member's database open for one socket. The returned function
// lets go of it; when the last one does, the database is uploaded.
func (m *Manager) Open(ctx context.Context, user string) (func(), error) {
	e := m.enter(user)
	defer m.leave(e)
	if e.db == nil {
		if err := m.load(ctx, e); err != nil {
			return nil, err
		}
	}
	e.sockets++
	if e.lease != nil {
		e.lease.Stop()
		e.lease = nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			e := m.enter(user)
			defer m.leave(e)
			e.sockets--
			m.settle(context.Background(), e)
		})
	}, nil
}

// Notes returns all the member saved. It fetches the database, and keeps it
// open for pageGrace unless a socket already holds it.
func (m *Manager) Notes(ctx context.Context, user string) ([]Note, error) {
	e := m.enter(user)
	defer m.leave(e)
	if e.db == nil {
		if err := m.load(ctx, e); err != nil {
			return nil, err
		}
	}
	if e.sockets == 0 {
		if e.lease != nil {
			e.lease.Stop()
		}
		e.lease = time.AfterFunc(pageGrace, func() { m.expire(user) })
	}
	rows, err := e.db.QueryContext(ctx, `select file, kind, data from notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.File, &n.Kind, &n.Data); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (m *Manager) expire(user string) {
	e := m.enter(user)
	defer m.leave(e)
	e.lease = nil
	m.settle(context.Background(), e)
}

// Save replaces what the member saved of one kind for one file. Empty data
// deletes it. The caller holds the database through Open.
func (m *Manager) Save(ctx context.Context, user, file, kind, data string) error {
	e := m.enter(user)
	defer m.leave(e)
	if e.db == nil {
		return errors.New("usernotes: database not open")
	}
	if data == "" {
		if _, err := e.db.ExecContext(ctx, `delete from notes where file = ?1 and kind = ?2`, file, kind); err != nil {
			return err
		}
		e.dirty = true
		return nil
	}
	// What the save replaces does not count against the quota.
	res, err := e.db.ExecContext(ctx, `
		insert into notes (file, kind, data, updated_at)
		select ?1, ?2, ?3, ?4
		where (select coalesce(sum(length(cast(data as blob))), 0) from notes
			where not (file = ?1 and kind = ?2)) + length(cast(?3 as blob)) <= ?5
		on conflict (file, kind) do update set data = excluded.data, updated_at = excluded.updated_at`,
		file, kind, data, time.Now().UnixMilli(), Quota)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrQuota
	}
	e.dirty = true
	return nil
}
