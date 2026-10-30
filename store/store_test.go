package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

// openTestStore opens a throwaway SQLite database in the test's temp dir.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "steki.db")
	s, err := Open(context.Background(), path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestLogReconciliationTablesAreGone(t *testing.T) {
	s := openTestStore(t)
	for _, table := range []string{"legacy_log_evidence", "log_scan_state"} {
		var n int
		if err := s.db.QueryRow(
			`select count(*) from sqlite_master where type = 'table' and name = ?1`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("table %s still exists", table)
		}
	}
}
