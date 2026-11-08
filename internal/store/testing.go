package store

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
)

// TB is the part of testing.TB the helpers use. Naming it here keeps package
// testing out of the services' binaries.
type TB interface {
	Helper()
	TempDir() string
	Fatal(args ...any)
	Cleanup(func())
}

// openTestBase opens a throwaway local SQLite database with a shard's schema. The
// statements behave as they do on a shard; s3db's own tests cover the bucket.
func openTestBase(t TB, shard string) base {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = db.Close() })
	b := base{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := b.migrate(context.Background(), shard); err != nil {
		t.Fatal(err)
	}
	return b
}

// OpenTestRegistry returns a throwaway Registry, for tests.
func OpenTestRegistry(t TB) *Registry { return &Registry{openTestBase(t, ShardRegistry)} }

// OpenTestGuard returns a throwaway Guard, for tests.
func OpenTestGuard(t TB) *Guard { return &Guard{openTestBase(t, ShardGuard)} }

// OpenTestActivity returns a throwaway Activity, for tests.
func OpenTestActivity(t TB) *Activity { return &Activity{openTestBase(t, ShardActivity)} }

// OpenTestLibrary returns a throwaway Library, for tests.
func OpenTestLibrary(t TB) *Library { return &Library{openTestBase(t, ShardLibrary)} }
