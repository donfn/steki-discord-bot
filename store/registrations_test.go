package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRegistrationLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertMemberJoin(ctx, "u1", "alice"); err != nil {
		t.Fatal(err)
	}
	reg, created, err := s.EnsureOpenRegistration(ctx, "u1", "g1")
	if err != nil || !created || reg.Step != StepAwaitingEmail || reg.ID == "" {
		t.Fatalf("first ensure = %+v, created=%v, %v", reg, created, err)
	}
	again, created, err := s.EnsureOpenRegistration(ctx, "u1", "g1")
	if err != nil || created || again.ID != reg.ID {
		t.Fatalf("second ensure = %+v, created=%v, %v", again, created, err)
	}

	if err := s.AttachChannel(ctx, reg.ID, "c1", "cat1"); err != nil {
		t.Fatal(err)
	}
	byChan, err := s.RegistrationByChannel(ctx, "c1")
	if err != nil || byChan.ID != reg.ID || byChan.CategoryID == nil || *byChan.CategoryID != "cat1" {
		t.Fatalf("by channel = %+v, %v", byChan, err)
	}

	exp := time.Now().Add(10 * time.Minute)
	if err := s.BeginCodeStep(ctx, reg.ID, "th1@edu.hmu.gr", "123456", exp); err != nil {
		t.Fatal(err)
	}
	got, _ := s.OpenRegistration(ctx, "u1")
	if got.Step != StepAwaitingCode || *got.Email != "th1@edu.hmu.gr" || got.CodeSentCount != 1 || !got.CodeValid(time.Now()) {
		t.Fatalf("after code step = %+v", got)
	}
	if got.CodeExpiresAt.Sub(exp).Abs() > time.Millisecond {
		t.Fatalf("code expiry %v, want %v", got.CodeExpiresAt, exp)
	}

	if n, err := s.IncrementFailedAttempts(ctx, reg.ID); err != nil || n != 1 {
		t.Fatalf("failed attempts = %d, %v", n, err)
	}
	if err := s.ResetToEmailStep(ctx, reg.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.OpenRegistration(ctx, "u1")
	if got.Step != StepAwaitingEmail || got.Email != nil || got.Code != nil || got.FailedAttempts != 0 {
		t.Fatalf("after reset = %+v", got)
	}

	if err := s.MarkVerified(ctx, reg.ID, "u1", " TH1@edu.hmu.gr "); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenRegistration(ctx, "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("registration still open after verification: %v", err)
	}
	m, err := s.GetMember(ctx, "u1")
	if err != nil || m.VerifiedAt == nil || m.VerifiedEmail == nil || *m.VerifiedEmail != "th1@edu.hmu.gr" {
		t.Fatalf("member after verify = %+v, %v", m, err)
	}

	// The closed row frees the partial unique index for a new attempt.
	next, created, err := s.EnsureOpenRegistration(ctx, "u1", "g1")
	if err != nil || !created || next.ID == reg.ID {
		t.Fatalf("new registration after close = %+v, created=%v, %v", next, created, err)
	}
}

func TestEnsureOpenRegistrationIsRaceFree(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.EnsureMember(ctx, "racer", "r"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	created, ids := 0, map[string]bool{}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg, made, err := s.EnsureOpenRegistration(ctx, "racer", "g1")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[reg.ID] = true
			if made {
				created++
			}
		}()
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Fatalf("created=%d distinct registrations=%d, want 1 and 1", created, len(ids))
	}
}

func TestMarkVerifiedRefusesAnAddressTakenByAnotherAccount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, u := range []string{"a", "b"} {
		if err := s.UpsertMemberJoin(ctx, u, u); err != nil {
			t.Fatal(err)
		}
	}
	ra, _, _ := s.EnsureOpenRegistration(ctx, "a", "g")
	rb, _, _ := s.EnsureOpenRegistration(ctx, "b", "g")

	if err := s.MarkVerified(ctx, ra.ID, "a", "x@edu.hmu.gr"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVerified(ctx, rb.ID, "b", "X@edu.hmu.gr"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("second claim = %v, want ErrEmailTaken", err)
	}
	// The refused claim must leave b unverified and its registration open.
	if m, _ := s.GetMember(ctx, "b"); m.VerifiedAt != nil {
		t.Fatal("refused claim still verified the member")
	}
	if _, err := s.OpenRegistration(ctx, "b"); err != nil {
		t.Fatalf("refused claim closed the registration: %v", err)
	}
}

func TestIdleCandidatesAndCloseOutcomes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.EnsureMember(ctx, "idle", "i"); err != nil {
		t.Fatal(err)
	}
	reg, _, _ := s.EnsureOpenRegistration(ctx, "idle", "g")
	if err := s.AttachChannel(ctx, reg.ID, "c9", ""); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.ListNudgeCandidates(ctx, time.Hour, 10); len(got) != 0 {
		t.Fatalf("a fresh registration is nudge candidate: %d", len(got))
	}
	// Age the row by hand: ten hours idle.
	if _, err := s.db.ExecContext(ctx, `update registrations set last_activity_at = ?1`,
		ms(time.Now().Add(-10*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListNudgeCandidates(ctx, time.Hour, 10); len(got) != 1 {
		t.Fatalf("nudge candidates = %d, want 1", len(got))
	}
	if err := s.MarkNudged(ctx, reg.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListNudgeCandidates(ctx, time.Hour, 10); len(got) != 0 {
		t.Fatal("a nudged registration is nudged again")
	}
	if got, _ := s.ListKickCandidates(ctx, 6*time.Hour, 10); len(got) != 1 {
		t.Fatalf("kick candidates = %d, want 1", len(got))
	}
	if err := s.Touch(ctx, reg.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListKickCandidates(ctx, 6*time.Hour, 10); len(got) != 0 {
		t.Fatal("a touched registration is still a kick candidate")
	}

	if err := s.CloseRegistration(ctx, reg.ID, OutcomeAbandonedKicked); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.ListOpen(ctx); len(open) != 0 {
		t.Fatalf("open after close = %d", len(open))
	}
	if err := s.IncrementKicked(ctx, "idle"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.GetMember(ctx, "idle"); m.KickedCount != 1 {
		t.Fatalf("kicked count = %d", m.KickedCount)
	}
}

func TestRejoinBumpsCountAndClearsLeft(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertMemberJoin(ctx, "u", "old")
	_ = s.MarkMemberLeft(ctx, "u")
	if m, _ := s.GetMember(ctx, "u"); m.LeftAt == nil {
		t.Fatal("left_at not stamped")
	}
	_ = s.UpsertMemberJoin(ctx, "u", "new")
	m, _ := s.GetMember(ctx, "u")
	if m.JoinCount != 2 || m.LeftAt != nil || m.Username != "new" {
		t.Fatalf("after rejoin = %+v", m)
	}
}

func TestEmailDedupAndRelease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertMemberJoin(ctx, "owner", "o")
	reg, _, _ := s.EnsureOpenRegistration(ctx, "owner", "g")
	if err := s.MarkVerified(ctx, reg.ID, "owner", "me@edu.hmu.gr"); err != nil {
		t.Fatal(err)
	}

	use, err := s.CheckEmail(ctx, "ME@edu.hmu.gr")
	if err != nil || !use.InUse || use.Legacy || !use.BlocksUser("other") || use.BlocksUser("owner") {
		t.Fatalf("check = %+v, %v", use, err)
	}
	if v, err := s.WhoIs(ctx, "me@edu.hmu.gr"); err != nil || v.Domain != "edu.hmu.gr" || *v.UserID != "owner" {
		t.Fatalf("whois = %+v, %v", v, err)
	}
	if list, _ := s.EmailsForUser(ctx, "owner"); len(list) != 1 {
		t.Fatalf("emails for user = %d", len(list))
	}

	// A legacy hash blocks the address it hashes from, and is freed with it.
	legacy := "old@edu.hmu.gr"
	if err := s.AddLegacyHash(ctx, LegacyHash(legacy), nil, "mongo"); err != nil {
		t.Fatal(err)
	}
	use, _ = s.CheckEmail(ctx, legacy)
	if !use.InUse || !use.Legacy || !use.BlocksUser("anyone") {
		t.Fatalf("legacy check = %+v", use)
	}
	if removed, err := s.RemoveVerifiedEmail(ctx, legacy); err != nil || !removed {
		t.Fatalf("remove legacy = %v, %v", removed, err)
	}
	if use, _ := s.CheckEmail(ctx, legacy); use.InUse {
		t.Fatal("legacy address still blocked after removal")
	}

	st, err := s.EmailStats(ctx)
	if err != nil || st.Verified != 1 || st.VerifiedLinked != 1 || st.LegacyHashes != 0 {
		t.Fatalf("stats = %+v, %v", st, err)
	}

	// Releasing frees the address and detaches it from the member.
	released, err := s.ClearVerification(ctx, "owner", true)
	if err != nil || released != 1 {
		t.Fatalf("release = %d, %v", released, err)
	}
	if use, _ := s.CheckEmail(ctx, "me@edu.hmu.gr"); use.InUse {
		t.Fatal("released address still blocked")
	}
	if m, _ := s.GetMember(ctx, "owner"); m.VerifiedAt != nil || m.VerifiedEmail != nil {
		t.Fatalf("member still verified: %+v", m)
	}
}

func TestVerifiedEmailDeleteDetachesMember(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertMemberJoin(ctx, "u", "u")
	reg, _, _ := s.EnsureOpenRegistration(ctx, "u", "g")
	_ = s.MarkVerified(ctx, reg.ID, "u", "z@edu.hmu.gr")

	if removed, err := s.RemoveVerifiedEmail(ctx, "z@edu.hmu.gr"); err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	// members.verified_email references verified_emails ON DELETE SET NULL.
	if m, _ := s.GetMember(ctx, "u"); m.VerifiedEmail != nil {
		t.Fatalf("dangling verified_email: %v", *m.VerifiedEmail)
	}
}

func TestAuditTrailRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.LogEvent(ctx, "u", "", EventJoined, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.LogEvent(ctx, "u", "reg-1", EventModAction,
		map[string]any{"action": "email_remove", "n": 2}, "mod9"); err != nil {
		t.Fatal(err)
	}
	hist, err := s.History(ctx, "u", 10)
	if err != nil || len(hist) != 2 {
		t.Fatalf("history = %d, %v", len(hist), err)
	}
	newest := hist[0]
	if newest.Kind != EventModAction || *newest.RegID != "reg-1" || *newest.ActorID != "mod9" ||
		newest.Detail["action"] != "email_remove" || newest.Detail["n"] != float64(2) {
		t.Fatalf("newest event = %+v", newest)
	}
	if hist[1].RegID != nil || hist[1].ActorID != nil || len(hist[1].Detail) != 0 {
		t.Fatalf("oldest event = %+v", hist[1])
	}

	counts, err := s.CountEventsByKind(ctx, time.Now().Add(-time.Hour))
	if err != nil || counts[EventJoined] != 1 || counts[EventModAction] != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	if counts, _ := s.CountEventsByKind(ctx, time.Now().Add(time.Hour)); len(counts) != 0 {
		t.Fatalf("counts from the future = %v", counts)
	}
}

func TestHelpAccessLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertMemberJoin(ctx, "h", "h")
	reg, _, _ := s.EnsureOpenRegistration(ctx, "h", "g")

	if ok, _ := s.HasHelpAccess(ctx, "h"); ok {
		t.Fatal("access before it was granted")
	}
	if err := s.RecordHelpAccess(ctx, "h"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHelpAccess(ctx, "h"); err != nil { // idempotent
		t.Fatal(err)
	}
	if ok, _ := s.HasHelpAccess(ctx, "h"); !ok {
		t.Fatal("access not recorded")
	}
	if stale, _ := s.StaleHelpAccess(ctx); len(stale) != 0 {
		t.Fatalf("stale while registration open: %v", stale)
	}
	_ = s.CloseRegistration(ctx, reg.ID, OutcomeVerified)
	if stale, _ := s.StaleHelpAccess(ctx); len(stale) != 1 || stale[0] != "h" {
		t.Fatalf("stale after close = %v", stale)
	}
	_ = s.ForgetHelpAccess(ctx, "h")
	if ok, _ := s.HasHelpAccess(ctx, "h"); ok {
		t.Fatal("access survived Forget")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := t.TempDir() + "/steki.db"
	ctx := context.Background()
	for range 2 {
		s, err := Open(ctx, path, nopLogger())
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}
