package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// importMarker is recorded in schema_migrations once Postgres has been copied,
// so the copy never runs twice and never needs the Postgres service again.
const importMarker = "import:postgres"

// pgTable describes one table to copy. cols are the SQLite column names;
// pgCols are the expressions that read them from Postgres, which differ only
// where a type needs casting to text (uuid, jsonb).
type pgTable struct {
	name   string
	cols   []string
	pgCols []string
	order  string
}

// pgTables are in dependency order: a row's references are copied before it.
var pgTables = []pgTable{
	{name: "verified_emails", order: "email",
		cols: []string{"email", "user_id", "domain", "source", "verified_at"}},
	{name: "legacy_email_hashes", order: "sha256",
		cols: []string{"sha256", "user_id", "source", "imported_at"}},
	{name: "members", order: "user_id",
		cols: []string{"user_id", "username", "first_seen_at", "last_join_at", "join_count",
			"left_at", "verified_at", "verified_email", "kicked_count"}},
	{name: "registrations", order: "created_at, id",
		cols: []string{"id", "user_id", "guild_id", "channel_id", "category_id", "step", "email",
			"code", "code_expires_at", "code_sent_count", "failed_attempts", "last_activity_at",
			"nudged_at", "created_at", "completed_at", "outcome"},
		pgCols: []string{"id::text", "user_id", "guild_id", "channel_id", "category_id", "step", "email",
			"code", "code_expires_at", "code_sent_count", "failed_attempts", "last_activity_at",
			"nudged_at", "created_at", "completed_at", "outcome"}},
	{name: "registration_events", order: "id",
		cols:   []string{"id", "user_id", "reg_id", "kind", "detail", "actor_id", "created_at"},
		pgCols: []string{"id", "user_id", "reg_id::text", "kind", "detail::text", "actor_id", "created_at"}},
	{name: "media_guard", order: "user_id",
		cols: []string{"user_id", "guild_id", "window_start", "exempt_until", "token_id",
			"restricted_at", "dm_channel_id", "dm_message_id", "updated_at"}},
	{name: "log_scan_state", order: "channel_id",
		cols: []string{"channel_id", "oldest_id", "newest_id", "complete", "scanned", "updated_at"}},
	{name: "legacy_log_evidence", order: "message_id, sha256, kind",
		cols: []string{"message_id", "sha256", "kind", "user_id", "email", "posted_at"}},
	{name: "help_access", order: "user_id",
		cols: []string{"user_id", "granted_at"}},
}

// ImportResult says what ImportPostgres did.
type ImportResult struct {
	Skipped string         // why nothing was copied, or empty if it ran
	Rows    map[string]int // rows copied per table
}

// ImportPostgres copies a Postgres database written by the previous version of
// the bot into this one, once. It does nothing when the copy was already made,
// and refuses to merge into a database that already holds data, since the two
// could then disagree about who owns an address.
//
// The Postgres side is read in one repeatable-read snapshot and the SQLite side
// written in one transaction, so a failure part-way leaves no trace and the
// next boot starts over.
func (s *Store) ImportPostgres(ctx context.Context, dsn string) (ImportResult, error) {
	var res ImportResult

	var done int
	if err := s.db.QueryRowContext(ctx,
		`select count(*) from schema_migrations where version = ?1`, importMarker).Scan(&done); err != nil {
		return res, err
	}
	if done > 0 {
		res.Skipped = "already imported"
		return res, nil
	}
	var existing int
	if err := s.db.QueryRowContext(ctx, `
		select (select count(*) from verified_emails)
		     + (select count(*) from legacy_email_hashes)
		     + (select count(*) from members)`).Scan(&existing); err != nil {
		return res, err
	}
	if existing > 0 {
		res.Skipped = "database already holds data"
		return res, nil
	}

	pg, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return res, fmt.Errorf("connect to postgres: %w", err)
	}
	defer pg.Close(context.Background())

	snap, err := pg.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return res, err
	}
	defer func() { _ = snap.Rollback(context.Background()) }()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	res.Rows = map[string]int{}
	for _, t := range pgTables {
		n, err := copyTable(ctx, snap, tx, t)
		if err != nil {
			return res, fmt.Errorf("copy %s: %w", t.name, err)
		}
		res.Rows[t.name] = n
	}

	if _, err := tx.ExecContext(ctx,
		`insert into schema_migrations (version, applied_at) values (?1, ?2)`,
		importMarker, ms(time.Now())); err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// copyTable streams one table from the Postgres snapshot into the SQLite
// transaction and checks that every row arrived.
func copyTable(ctx context.Context, snap pgx.Tx, tx *sql.Tx, t pgTable) (int, error) {
	pgCols := t.pgCols
	if pgCols == nil {
		pgCols = t.cols
	}

	var want int
	if err := snap.QueryRow(ctx, `select count(*) from `+t.name).Scan(&want); err != nil {
		return 0, err
	}

	rows, err := snap.Query(ctx, fmt.Sprintf(`select %s from %s order by %s`,
		strings.Join(pgCols, ", "), t.name, t.order))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	marks := make([]string, len(t.cols))
	for i := range marks {
		marks[i] = fmt.Sprintf("?%d", i+1)
	}
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`insert into %s (%s) values (%s)`,
		t.name, strings.Join(t.cols, ", "), strings.Join(marks, ", ")))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return n, err
		}
		for i, v := range vals {
			if at, ok := v.(time.Time); ok {
				vals[i] = ms(at)
			}
		}
		if _, err := stmt.ExecContext(ctx, vals...); err != nil {
			return n, fmt.Errorf("row %d: %w", n+1, err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	if n != want {
		return n, fmt.Errorf("copied %d rows, postgres counted %d", n, want)
	}

	var got int
	if err := tx.QueryRowContext(ctx, `select count(*) from `+t.name).Scan(&got); err != nil {
		return n, err
	}
	if got != want {
		return n, fmt.Errorf("sqlite holds %d rows afterwards, postgres had %d", got, want)
	}
	return n, nil
}
