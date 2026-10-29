// Package store is the bot's persistence layer, backed by an embedded SQLite
// database file.
//
// Every exported method takes a context and runs a single statement or an
// explicit transaction; there is no ORM and no query builder. Callers are
// expected to pass a context with a deadline — Handler does this via WithTimeout.
//
// SQLite runs inside the bot's own process, so there is no database server to
// keep resident: the whole footprint is the page cache of the two connections
// below, a megabyte each. Timestamps are stored as integer Unix milliseconds.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("store: not found")

	// ErrEmailTaken is returned when an address is already bound to a
	// different account.
	ErrEmailTaken = errors.New("store: email already used by another account")
)

// Store owns the database handle.
type Store struct {
	db   *sql.DB
	path string
	log  *slog.Logger
}

// pragmas are applied to every connection the pool opens.
var pragmas = []string{
	"busy_timeout(5000)",
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
	"foreign_keys(1)",
	// 1 MiB of page cache per connection (a negative size is in KiB), and no
	// memory-mapped I/O, so the cache is the only memory the file costs.
	"cache_size(-1024)",
	"mmap_size(0)",
	// Keep the write-ahead log from growing past a few megabytes on disk.
	"journal_size_limit(4194304)",
	"wal_autocheckpoint(200)",
}

// Open creates the database file and its directory when they do not exist,
// then applies any pending migrations.
func Open(ctx context.Context, path string, logger *slog.Logger) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	q := url.Values{"_pragma": pragmas, "_txlock": {"immediate"}}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// Writers are serialised by SQLite anyway; a second connection lets reads
	// proceed beside one. More would only cost memory.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: path, log: logger}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database unreachable: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// DBStats reports connection usage, for /bot stats.
func (s *Store) DBStats() sql.DBStats { return s.db.Stats() }

// FileSize is the database file's size on disk, write-ahead log included.
func (s *Store) FileSize() int64 {
	var n int64
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// Close checkpoints and closes the database.
func (s *Store) Close() { _ = s.db.Close() }

// Ping reports whether the database is currently usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// migrate applies every embedded migration that has not been applied yet,
// in filename order, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		create table if not exists schema_migrations (
			version    text primary key,
			applied_at integer not null
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `select version from schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`insert into schema_migrations (version, applied_at) values (?1, ?2)`,
			name, ms(time.Now())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		s.log.Info("applied migration", "version", name)
	}
	return nil
}

// noRows maps database/sql's sentinel onto the package's own.
func noRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// scanner is what *sql.Row and *sql.Rows have in common.
type scanner interface{ Scan(dest ...any) error }

// ms converts a time to the integer stored in the database.
func ms(t time.Time) int64 { return t.UnixMilli() }

// msPtr is ms for an optional time; nil becomes NULL.
func msPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// nullStr turns an empty string into NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// timeDest scans a NOT NULL millisecond column into a time.
type timeDest struct{ dst *time.Time }

func (d timeDest) Scan(v any) error {
	n, ok := v.(int64)
	if !ok {
		return fmt.Errorf("store: timestamp column holds %T, want integer", v)
	}
	*d.dst = time.UnixMilli(n).UTC()
	return nil
}

// nullTimeDest scans a nullable millisecond column into an optional time.
type nullTimeDest struct{ dst **time.Time }

func (d nullTimeDest) Scan(v any) error {
	if v == nil {
		*d.dst = nil
		return nil
	}
	var t time.Time
	if err := (timeDest{&t}).Scan(v); err != nil {
		return err
	}
	*d.dst = &t
	return nil
}

// newID returns a random version 4 UUID, the form registration ids have
// always had.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system random source failing is not recoverable
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
