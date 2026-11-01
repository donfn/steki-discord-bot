package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"steki-bot/s3db"
)

// memBucket is just enough of a bucket for the split.
type memBucket struct {
	mu sync.Mutex
	m  map[string][]byte
	n  int
}

func (b *memBucket) Get(_ context.Context, k string, w io.Writer) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.m[k]
	if !ok {
		return "", s3db.ErrNotFound
	}
	_, err := w.Write(v)
	return string(rune('a' + len(v)%26)), err
}

func (b *memBucket) Put(_ context.Context, k string, r io.Reader, _ int64) error {
	v, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[k] = v
	return nil
}

func (b *memBucket) PutIf(_ context.Context, k string, v []byte, etag string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.m[k]; ok && etag == "" {
		return "", s3db.ErrConflict
	}
	b.m[k] = bytes.Clone(v)
	return string(rune('a' + len(v)%26)), nil
}

func (b *memBucket) Delete(_ context.Context, k string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, k)
	return nil
}

func (b *memBucket) List(_ context.Context, p string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for k := range b.m {
		if strings.HasPrefix(k, p) {
			out = append(out, k)
		}
	}
	return out, nil
}

// legacyDB builds an old-style single database holding every shard's tables.
func legacyDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "steki.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, dir := range []string{ShardRegistry, ShardGuard, ShardActivity, ShardLibrary} {
		entries, err := migrationFS.ReadDir("migrations/" + dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			body, _ := migrationFS.ReadFile("migrations/" + dir + "/" + e.Name())
			// Every shard has an events table; the old database had one.
			if dir != ShardRegistry && strings.Contains(string(body), "create table registration_events") {
				text := string(body)
				i := strings.Index(text, "-- Append-only audit trail")
				if i < 0 {
					continue
				}
				body = []byte(text[:i])
			}
			if _, err := db.ExecContext(ctx, string(body)); err != nil {
				t.Fatalf("%s/%s: %v", dir, e.Name(), err)
			}
		}
	}
	for _, q := range []string{
		`insert into verified_emails (email, user_id, domain) values ('th1@edu.hmu.gr', 'u1', 'edu.hmu.gr')`,
		`insert into members (user_id, username) values ('u1', 'one')`,
		`insert into registration_events (user_id, kind) values ('u1', 'joined')`,
		`insert into banned_emails (hash, banned_at) values ('h', 1)`,
		`insert into media_guard (user_id, guild_id) values ('u1', 'g')`,
		`insert into member_activity (user_id, joined_at, synced_at) values ('u1', 1, 1)`,
		`insert into stickies (channel_id, message_id, posted_at) values ('c', 'm', 1)`,
		`insert into ip_bans (ip, kind, reason, started_at, until) values ('1.1.1.1', 'ban', 'r', 1, 2)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return path
}

func TestSplitLegacyPutsEveryRowInItsShard(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bucket := &memBucket{m: map[string][]byte{}}
	legacy := legacyDB(t)
	scratch := t.TempDir()
	if err := SplitLegacy(ctx, legacy, bucket, scratch, log); err != nil {
		t.Fatal(err)
	}
	// Running it again must not touch what is there.
	before := len(bucket.m)
	if err := SplitLegacy(ctx, legacy, bucket, scratch, log); err != nil {
		t.Fatal(err)
	}
	if len(bucket.m) != before {
		t.Errorf("a second split changed the bucket: %d objects, was %d", len(bucket.m), before)
	}
	for shard, want := range map[string]map[string]int{
		ShardRegistry: {"verified_emails": 1, "members": 1, "registration_events": 1, "banned_emails": 1},
		ShardGuard:    {"media_guard": 1, "registration_events": 0},
		ShardActivity: {"member_activity": 1},
		ShardLibrary:  {"stickies": 1, "ip_bans": 1, "registration_events": 0},
	} {
		sh, err := s3db.Open(ctx, s3db.Options{Name: shard, Dir: filepath.Join(t.TempDir(), shard), Objects: bucket, Owner: "t", Lazy: 1 << 40})
		if err != nil {
			t.Fatalf("%s: %v", shard, err)
		}
		b := base{db: sh.DB(), log: log}
		if err := b.migrate(ctx, shard); err != nil {
			t.Fatalf("%s: migrate over the split: %v", shard, err)
		}
		for table, n := range want {
			var got int
			if err := sh.DB().QueryRow(`select count(*) from ` + table).Scan(&got); err != nil {
				t.Fatalf("%s.%s: %v", shard, table, err)
			}
			if got != n {
				t.Errorf("%s.%s has %d rows, want %d", shard, table, got, n)
			}
		}
		_ = sh.Close()
	}
}

func TestSplitLegacyLeavesNothingBehindOnAMissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`create table x (y)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	bucket := &memBucket{m: map[string][]byte{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err = SplitLegacy(context.Background(), path, bucket, t.TempDir(), log)
	if err == nil {
		t.Fatal("split a database without the tables")
	}
	if ok, cerr := s3db.Exists(context.Background(), bucket, ShardRegistry); cerr != nil || ok {
		t.Fatalf("a failed split published a shard: %v %v", ok, errors.Join(cerr))
	}
}
