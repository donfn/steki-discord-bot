// Package store is the bot's persistence layer, backed by PostgreSQL.
//
// Every exported method takes a context and runs a single statement or an
// explicit transaction; there is no ORM and no query builder. Callers are
// expected to pass a context with a deadline — Handler does this via WithTimeout.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is an arbitrary but stable key for pg_advisory_lock, so that
// two instances booting at once cannot run migrations concurrently.
const migrationLockID int64 = 8_123_451_337

var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("store: not found")

	// ErrEmailTaken is returned when an address is already bound to a
	// different account.
	ErrEmailTaken = errors.New("store: email already used by another account")
)

// Store owns the connection pool.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// Open connects to Postgres, verifies the connection and applies any pending
// migrations. It blocks until the database is reachable or ctx expires, which
// matters on Railway where the bot and the database may start together.
func Open(ctx context.Context, dsn string, logger *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	s := &Store{pool: pool, log: logger}

	if err := s.waitReady(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// waitReady retries Ping with backoff until the database answers or ctx ends.
func (s *Store) waitReady(ctx context.Context) error {
	delay := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := s.pool.Ping(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("database unreachable: %w", err)
		}
		s.log.Warn("database not ready, retrying", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("database unreachable: %w", err)
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
}

// PoolStat reports connection pool usage, for /bot stats.
func (s *Store) PoolStat() *pgxpool.Stat { return s.pool.Stat() }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is currently reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// migrate applies every embedded migration that has not been applied yet,
// in filename order, each in its own transaction, under an advisory lock.
func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `select pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := conn.Exec(ctx, `
		create table if not exists schema_migrations (
			version    text primary key,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := conn.Query(ctx, `select version from schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
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
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`insert into schema_migrations (version) values ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		s.log.Info("applied migration", "version", name)
	}
	return nil
}

// noRows maps pgx's sentinel onto the package's own.
func noRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
