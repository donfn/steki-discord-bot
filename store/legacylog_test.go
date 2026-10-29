package store

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("STEKI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("STEKI_TEST_DATABASE_URL not set")
	}
	s, err := Open(context.Background(), dsn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestLogScanStateRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ch := "test-channel-" + time.Now().Format(time.RFC3339Nano)

	st, err := s.LogScanState(ctx, ch)
	if err != nil || st.OldestID != "" || st.Complete {
		t.Fatalf("fresh state = %+v, %v", st, err)
	}
	st.OldestID, st.NewestID, st.Scanned = "100", "200", 100
	ev := []LogEvidence{{MessageID: "150", Hash: "h1", Kind: EvidenceSubmitted, UserID: "u1", PostedAt: time.Now()}}
	if err := s.SaveLogPage(ctx, st, ev); err != nil {
		t.Fatal(err)
	}
	// Saving the same page again must not duplicate evidence.
	if err := s.SaveLogPage(ctx, st, ev); err != nil {
		t.Fatal(err)
	}
	got, err := s.LogScanState(ctx, ch)
	if err != nil || got.OldestID != "100" || got.NewestID != "200" || got.Scanned != 100 {
		t.Fatalf("saved state = %+v, %v", got, err)
	}
	byHash, err := s.EvidenceFor(ctx, []string{"h1"})
	if err != nil || len(byHash["h1"]) != 1 || byHash["h1"][0].UserID != "u1" {
		t.Fatalf("evidence = %+v, %v", byHash, err)
	}
}

func TestResolveClaimBindsOwnerAndPlaintext(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const owner = "900100000000000001"
	email := "th555@edu.hmu.gr"
	hash := LegacyHash(email)
	_, _ = s.RemoveVerifiedEmail(ctx, email)

	if err := s.AddLegacyHash(ctx, hash, nil, SourceDiscordLog); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveClaim(ctx, LegacyClaim{Hash: hash}, owner, ""); err != nil {
		t.Fatal(err)
	}
	use, err := s.CheckEmail(ctx, email)
	if err != nil || !use.Legacy || use.UserID == nil || *use.UserID != owner {
		t.Fatalf("after owner = %+v, %v", use, err)
	}

	// Finding the plaintext turns the hash into a verified address.
	if err := s.ResolveClaim(ctx, LegacyClaim{Hash: hash, UserID: use.UserID}, "", email); err != nil {
		t.Fatal(err)
	}
	use, err = s.CheckEmail(ctx, email)
	if err != nil || use.Legacy || use.UserID == nil || *use.UserID != owner {
		t.Fatalf("after plaintext = %+v, %v", use, err)
	}
}

func TestClearVerification(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const user = "900100000000000002"
	email := "th556@edu.hmu.gr"
	_, _ = s.RemoveVerifiedEmail(ctx, email)

	if err := s.EnsureMember(ctx, user, "test"); err != nil {
		t.Fatal(err)
	}
	reg, _, err := s.EnsureOpenRegistration(ctx, user, "g")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVerified(ctx, reg.ID, user, email); err != nil {
		t.Fatal(err)
	}

	// Without release the member keeps their claim on the address.
	if n, err := s.ClearVerification(ctx, user, false); err != nil || n != 0 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	m, err := s.GetMember(ctx, user)
	if err != nil || m.VerifiedAt != nil || m.VerifiedEmail != nil {
		t.Fatalf("member still verified: %+v, %v", m, err)
	}
	if use, _ := s.CheckEmail(ctx, email); use.BlocksUser(user) || !use.BlocksUser("someone-else") {
		t.Fatalf("claim after clear = %+v", use)
	}

	// With release the address is free for anyone.
	if n, err := s.ClearVerification(ctx, user, true); err != nil || n != 1 {
		t.Fatalf("release = %d, %v", n, err)
	}
	if use, _ := s.CheckEmail(ctx, email); use.InUse {
		t.Fatalf("address still claimed: %+v", use)
	}
}
