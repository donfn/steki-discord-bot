// Package store is the services' persistence layer: SQLite databases kept in the
// bucket by package s3db, one per concern, each written by a single service.
//
// Each database has its own Go type, so a service holding one cannot even compile
// a query against another's tables. Across services data moves only through
// their internal APIs.
//
// Every exported method takes a context and runs a single statement or an
// explicit transaction; there is no ORM and no query builder. Timestamps are
// stored as integer Unix milliseconds.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"steki-bot/s3db"
)

//go:embed migrations
var migrationFS embed.FS

var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("store: not found")

	// ErrEmailTaken is returned when an address is already bound to a
	// different account.
	ErrEmailTaken = errors.New("store: email already used by another account")
)

// base is what every shard's type shares: the handle, the audit trail and the
// health checks.
type base struct {
	db   *sql.DB
	size func() int64
	log  *slog.Logger
}

// DBStats reports connection usage, for /bot stats.
func (s *base) DBStats() sql.DBStats { return s.db.Stats() }

// FileSize is the database's size on disk, write-ahead log included.
func (s *base) FileSize() int64 {
	if s.size == nil {
		return 0
	}
	return s.size()
}

// Ping reports whether the database is currently usable.
func (s *base) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Registry is the registration service's shard: members, their registrations,
// the addresses they used, and the addresses closed to them.
type Registry struct{ base }

// Guard is the moderation service's shard: the media guard's state.
type Guard struct{ base }

// Activity is the moderation service's index of when members last spoke. It
// changes on every message and losing its last moments costs nothing, so it is
// apart from Guard, on a shard that does not hold statements back for the bucket.
type Activity struct{ base }

// Library is the web service's shard: course material, reports and IP blocks.
type Library struct{ base }

// Shard names, which are also their directories under db/ in the bucket and
// under migrations/ here.
const (
	ShardRegistry = "registry"
	ShardGuard    = "guard"
	ShardActivity = "activity"
	ShardLibrary  = "library"
)

// ShardOptions says how to reach the bucket.
type ShardOptions struct {
	Objects s3db.Objects
	Dir     string // scratch directory, one subdirectory per shard
	Owner   string // this deployment
	// CreateEmpty makes a shard that is not in the bucket. Only a fresh local
	// setup wants this: in production an absent shard must stop the service, as
	// an empty registry would let members reuse addresses that are taken.
	CreateEmpty bool
	OnFenced    func()
	Log         *slog.Logger
}

// activityShipEvery is how much of the activity index a crash can lose.
const activityShipEvery = 30 * time.Second

func openShard(ctx context.Context, name string, lazy time.Duration, o ShardOptions) (*s3db.Shard, base, error) {
	sh, err := s3db.Open(ctx, s3db.Options{
		Name: name, Dir: filepath.Join(o.Dir, name), Objects: o.Objects, Log: o.Log,
		Owner: o.Owner, Lazy: lazy, Create: o.CreateEmpty, OnFenced: o.OnFenced,
	})
	if err != nil {
		return nil, base{}, err
	}
	b := base{db: sh.DB(), size: sh.FileSize, log: o.Log}
	if err := b.migrate(ctx, name); err != nil {
		_ = sh.Close()
		return nil, base{}, fmt.Errorf("migrate %s: %w", name, err)
	}
	return sh, b, nil
}

// OpenRegistry opens the registration service's shard.
func OpenRegistry(ctx context.Context, o ShardOptions) (*Registry, *s3db.Shard, error) {
	sh, b, err := openShard(ctx, ShardRegistry, 0, o)
	if err != nil {
		return nil, nil, err
	}
	return &Registry{b}, sh, nil
}

// OpenGuard opens the media guard's shard.
func OpenGuard(ctx context.Context, o ShardOptions) (*Guard, *s3db.Shard, error) {
	sh, b, err := openShard(ctx, ShardGuard, 0, o)
	if err != nil {
		return nil, nil, err
	}
	return &Guard{b}, sh, nil
}

// OpenActivity opens the activity index's shard.
func OpenActivity(ctx context.Context, o ShardOptions) (*Activity, *s3db.Shard, error) {
	sh, b, err := openShard(ctx, ShardActivity, activityShipEvery, o)
	if err != nil {
		return nil, nil, err
	}
	return &Activity{b}, sh, nil
}

// OpenLibrary opens the web service's shard.
func OpenLibrary(ctx context.Context, o ShardOptions) (*Library, *s3db.Shard, error) {
	sh, b, err := openShard(ctx, ShardLibrary, 0, o)
	if err != nil {
		return nil, nil, err
	}
	return &Library{b}, sh, nil
}

// migrate applies every embedded migration that has not been applied yet,
// in filename order, each in its own transaction.
func (s *base) migrate(ctx context.Context, shard string) error {
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

	entries, err := migrationFS.ReadDir("migrations/" + shard)
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
		body, err := migrationFS.ReadFile("migrations/" + shard + "/" + name)
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
		s.log.Info("applied migration", "shard", shard, "version", name)
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
