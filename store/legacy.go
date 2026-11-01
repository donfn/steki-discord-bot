package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"steki-bot/s3db"
)

// legacyTables says where each table of the old single database went.
var legacyTables = map[string][]string{
	ShardRegistry: {"verified_emails", "legacy_email_hashes", "members", "registrations",
		"registration_events", "help_access", "banned_emails"},
	ShardGuard:    {"media_guard"},
	ShardActivity: {"member_activity"},
	ShardLibrary:  {"materials", "material_scan", "stickies", "material_reports", "study_notes", "ip_bans"},
}

// SplitLegacy publishes the old single database at path as the shards, skipping
// any shard the bucket already has, so running it again changes nothing. The old
// file is only read. Nothing may be writing to it, which holds because the old
// bot's deployment gives up the volume before this one gets it.
func SplitLegacy(ctx context.Context, path string, o s3db.Objects, scratch string, log *slog.Logger) error {
	for _, shard := range []string{ShardRegistry, ShardGuard, ShardActivity, ShardLibrary} {
		exists, err := s3db.Exists(ctx, o, shard)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		dir := filepath.Join(scratch, "split-"+shard)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		out := filepath.Join(dir, shard+".db")
		rows, err := buildShard(ctx, path, out, shard, log)
		if err != nil {
			return fmt.Errorf("split %s: %w", shard, err)
		}
		published, err := s3db.Seed(ctx, o, shard, out)
		if err != nil {
			return fmt.Errorf("publish %s: %w", shard, err)
		}
		_ = os.RemoveAll(dir)
		log.Warn("split the old database into a shard", "shard", shard, "rows", rows, "published", published)
	}
	return nil
}

// buildShard creates the shard's file with its schema and copies its tables.
func buildShard(ctx context.Context, legacy, out, shard string, log *slog.Logger) (map[string]int64, error) {
	q := url.Values{"_pragma": {"journal_mode(WAL)", "foreign_keys(0)", "busy_timeout(5000)"}}
	db, err := sql.Open("sqlite", "file:"+out+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // an attachment belongs to one connection

	b := base{db: db, log: log}
	if err := b.migrate(ctx, shard); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `attach database ?1 as legacy`, legacy); err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	counts := map[string]int64{}
	for _, table := range legacyTables[shard] {
		cols, err := tableColumns(ctx, tx, table)
		if err != nil {
			return nil, err
		}
		list := strings.Join(cols, ", ")
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`insert into main.%s (%s) select %s from legacy.%s`,
			table, list, list, table)); err != nil {
			return nil, fmt.Errorf("copy %s: %w", table, err)
		}
		var have, want int64
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`select count(*) from main.%s`, table)).Scan(&have); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`select count(*) from legacy.%s`, table)).Scan(&want); err != nil {
			return nil, err
		}
		if have != want {
			return nil, fmt.Errorf("copy %s: %d rows, the old database has %d", table, have, want)
		}
		counts[table] = have
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `detach database legacy`); err != nil {
		return nil, err
	}
	// Closing the last connection folds the log into the file and removes it.
	if _, err := db.ExecContext(ctx, `pragma wal_checkpoint(truncate)`); err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return counts, nil
}

// tableColumns lists a table's columns as the new schema has them.
func tableColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `select name from pragma_table_info(?1)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("table %s has no columns", table)
	}
	return out, nil
}
