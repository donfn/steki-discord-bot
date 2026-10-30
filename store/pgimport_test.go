package store

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed testdata/postgres_schema.sql
var postgresSchema string

// pgSeed is a small database in the shape the Postgres-era bot left behind.
const pgSeed = `
insert into verified_emails (email, user_id, domain, source, verified_at) values
  ('alice@edu.hmu.gr', 'u1', 'edu.hmu.gr', 'live',        '2026-03-01 10:00:00+00'),
  ('ghost@csd.uoc.gr', null, 'csd.uoc.gr', 'discord_log', '2026-03-02 11:30:15.123+00');
insert into legacy_email_hashes (sha256, user_id, source, imported_at) values
  ('aaaa', 'u9', 'mongo', '2026-02-01 00:00:00+00'),
  ('bbbb', null, 'mongo', '2026-02-01 00:00:00+00');
insert into members (user_id, username, first_seen_at, last_join_at, join_count, left_at, verified_at, verified_email, kicked_count) values
  ('u1', 'alice', '2026-03-01 09:00:00+00', '2026-03-01 09:00:00+00', 2, null, '2026-03-01 10:00:00+00', 'alice@edu.hmu.gr', 1),
  ('u2', 'bob',   '2026-03-05 09:00:00+00', '2026-03-06 09:00:00+00', 1, '2026-03-07 00:00:00+00', null, null, 0);
insert into registrations (id, user_id, guild_id, channel_id, category_id, step, email, code, code_expires_at, code_sent_count, failed_attempts, last_activity_at, nudged_at, created_at, completed_at, outcome) values
  ('11111111-1111-4111-8111-111111111111', 'u1', 'g1', 'c1', 'k1', 'done', 'alice@edu.hmu.gr', null, null, 1, 0, '2026-03-01 10:00:00+00', null, '2026-03-01 09:00:00+00', '2026-03-01 10:00:00+00', 'verified'),
  ('22222222-2222-4222-8222-222222222222', 'u2', 'g1', 'c2', null, 'awaiting_code', 'bob@edu.hmu.gr', '654321', '2026-03-06 09:10:00+00', 1, 2, '2026-03-06 09:05:00+00', '2026-03-06 10:05:00+00', '2026-03-06 09:00:00+00', null, null);
insert into registration_events (user_id, reg_id, kind, detail, actor_id, created_at) values
  ('u1', '11111111-1111-4111-8111-111111111111', 'verified', '{"via":"code"}', null, '2026-03-01 10:00:00+00'),
  ('u2', null, 'mod_action', '{"action":"email_remove","n":3}', 'mod1', '2026-03-08 12:00:00+00');
insert into media_guard (user_id, guild_id, window_start, exempt_until, token_id, restricted_at, dm_channel_id, dm_message_id, updated_at) values
  ('u2', 'g1', null, null, 'tok', '2026-03-09 00:00:00+00', 'dm1', 'm1', '2026-03-09 00:00:00+00');
insert into log_scan_state (channel_id, oldest_id, newest_id, complete, scanned, updated_at) values
  ('log', '100', '200', true, 5000, '2026-03-10 00:00:00+00');
insert into legacy_log_evidence (message_id, sha256, kind, user_id, email, posted_at) values
  ('150', 'aaaa', 'submitted', 'u9', null, '2025-12-01 00:00:00+00');
insert into help_access (user_id, granted_at) values ('u2', '2026-03-06 09:30:00+00');
`

// TestImportPostgres needs a disposable Postgres, named by
// STEKI_TEST_POSTGRES_URL; its public schema is wiped.
func TestImportPostgres(t *testing.T) {
	dsn := os.Getenv("STEKI_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("STEKI_TEST_POSTGRES_URL not set")
	}
	ctx := context.Background()

	pg, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close(ctx)
	for _, q := range []string{`drop schema public cascade`, `create schema public`, postgresSchema, pgSeed} {
		if _, err := pg.Exec(ctx, q); err != nil {
			t.Fatalf("seed postgres: %v\n%s", err, q)
		}
	}

	s := openTestStore(t)
	res, err := s.ImportPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"verified_emails": 2, "legacy_email_hashes": 2, "members": 2, "registrations": 2,
		"registration_events": 2, "media_guard": 1, "help_access": 1}
	for table, n := range want {
		if res.Rows[table] != n {
			t.Errorf("%s: copied %d rows, want %d", table, res.Rows[table], n)
		}
	}

	use, err := s.CheckEmail(ctx, "alice@edu.hmu.gr")
	if err != nil || !use.InUse || use.UserID == nil || *use.UserID != "u1" {
		t.Fatalf("alice's address = %+v, %v", use, err)
	}
	ghost, err := s.WhoIs(ctx, "ghost@csd.uoc.gr")
	if err != nil || ghost.UserID != nil || ghost.Source != SourceDiscordLog ||
		!ghost.VerifiedAt.Equal(time.Date(2026, 3, 2, 11, 30, 15, 123_000_000, time.UTC)) {
		t.Fatalf("ghost = %+v, %v", ghost, err)
	}
	if use, _ := s.CheckEmail(ctx, "someone@x"); use.InUse {
		t.Fatal("an unknown address is in use")
	}

	m, err := s.GetMember(ctx, "u1")
	if err != nil || m.JoinCount != 2 || m.KickedCount != 1 || m.VerifiedEmail == nil || *m.VerifiedEmail != "alice@edu.hmu.gr" {
		t.Fatalf("member u1 = %+v, %v", m, err)
	}
	bob, _ := s.GetMember(ctx, "u2")
	if bob.LeftAt == nil || !bob.LeftAt.Equal(time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)) || bob.VerifiedAt != nil {
		t.Fatalf("member u2 = %+v", bob)
	}

	reg, err := s.OpenRegistration(ctx, "u2")
	if err != nil || reg.ID != "22222222-2222-4222-8222-222222222222" || reg.Step != StepAwaitingCode ||
		*reg.Code != "654321" || reg.FailedAttempts != 2 || reg.NudgedAt == nil || reg.Outcome != nil ||
		!reg.CodeExpiresAt.Equal(time.Date(2026, 3, 6, 9, 10, 0, 0, time.UTC)) {
		t.Fatalf("open registration = %+v, %v", reg, err)
	}
	if _, err := s.OpenRegistration(ctx, "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("u1's closed registration came across open: %v", err)
	}

	hist, err := s.History(ctx, "u2", 5)
	if err != nil || len(hist) != 1 || hist[0].Detail["action"] != "email_remove" || *hist[0].ActorID != "mod1" || hist[0].RegID != nil {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	// New events must continue after the imported ids rather than collide.
	if err := s.LogEvent(ctx, "u2", "", EventJoined, nil, ""); err != nil {
		t.Fatal(err)
	}

	if g, err := s.GuardStatus(ctx, "u2"); err != nil || !g.LinkPending || g.RestrictedAt == nil {
		t.Fatalf("guard = %+v, %v", g, err)
	}
	if ok, _ := s.GuardTokenActive(ctx, "u2", "tok"); !ok {
		t.Fatal("outstanding guard link lost")
	}
	if ok, _ := s.HasHelpAccess(ctx, "u2"); !ok {
		t.Fatal("help access lost")
	}
	if es, _ := s.EmailStats(ctx); es.Verified != 2 || es.VerifiedLinked != 1 || es.LegacyHashes != 2 {
		t.Fatalf("stats = %+v", es)
	}

	// A second boot must not copy again, even with Postgres unreachable.
	again, err := s.ImportPostgres(ctx, "postgres://nobody@127.0.0.1:1/none")
	if err != nil || again.Skipped != "already imported" {
		t.Fatalf("second import = %+v, %v", again, err)
	}
}

func TestImportPostgresRefusesANonEmptyDatabase(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.UpsertMemberJoin(ctx, "u", "u"); err != nil {
		t.Fatal(err)
	}
	res, err := s.ImportPostgres(ctx, "postgres://nobody@127.0.0.1:1/none")
	if err != nil || res.Skipped != "database already holds data" {
		t.Fatalf("import into a populated database = %+v, %v", res, err)
	}
}

func TestImportPostgresFailureLeavesNoMarker(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.ImportPostgres(ctx, "postgres://nobody@127.0.0.1:1/none"); err == nil {
		t.Fatal("import from an unreachable postgres reported success")
	}
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`select count(*) from schema_migrations where version = ?1`, importMarker).Scan(&n); err != nil || n != 0 {
		t.Fatalf("marker rows after a failed import = %d, %v", n, err)
	}
}
