package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"  TH123@EDU.HMU.GR ": "th123@edu.hmu.gr",
		"csd456@csd.uoc.gr":   "csd456@csd.uoc.gr",
	}
	for in, want := range cases {
		if got := NormalizeEmail(in); got != want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmailDomain(t *testing.T) {
	if got := EmailDomain("th123@edu.hmu.gr"); got != "edu.hmu.gr" {
		t.Errorf("EmailDomain = %q", got)
	}
	if got := EmailDomain("not-an-email"); got != "" {
		t.Errorf("EmailDomain(no @) = %q, want empty", got)
	}
}

func TestLegacyHashStableAndCaseInsensitive(t *testing.T) {
	// Known SHA-256 of "th123@edu.hmu.gr".
	a := LegacyHash("th123@edu.hmu.gr")
	b := LegacyHash("  TH123@EDU.HMU.GR  ")
	if a != b {
		t.Errorf("hash not normalization-stable: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("hash length = %d, want 64", len(a))
	}
}

func TestEmailUseBlocksUser(t *testing.T) {
	owner := "1"
	cases := []struct {
		name string
		use  EmailUse
		user string
		want bool
	}{
		{"unused", EmailUse{}, "1", false},
		{"own address on rejoin", EmailUse{InUse: true, UserID: &owner}, "1", false},
		{"someone else's", EmailUse{InUse: true, UserID: &owner}, "2", true},
		{"legacy, owner unknown", EmailUse{InUse: true, Legacy: true}, "1", true},
	}
	for _, c := range cases {
		if got := c.use.BlocksUser(c.user); got != c.want {
			t.Errorf("%s: BlocksUser = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsLegacyHash(t *testing.T) {
	if !IsLegacyHash(" " + LegacyHash("th123@edu.hmu.gr") + " ") {
		t.Error("a real digest was not recognised")
	}
	for _, in := range []string{"", "th123@edu.hmu.gr", LegacyHash("a")[:63], LegacyHash("a")[:63] + "z"} {
		if IsLegacyHash(in) {
			t.Errorf("IsLegacyHash(%q) = true", in)
		}
	}
}

// verifiedMember makes userID a verified member holding email.
func verifiedMember(t *testing.T, s *Store, userID, email string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertMemberJoin(ctx, userID, userID); err != nil {
		t.Fatal(err)
	}
	reg, _, err := s.EnsureOpenRegistration(ctx, userID, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVerified(ctx, reg.ID, userID, email); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateVerifiedEmailCorrectsTheAddress(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	verifiedMember(t, s, "u1", "th1@edu.hmu.gr")

	got, err := s.UpdateVerifiedEmail(ctx, " TH1@edu.hmu.gr ", EmailEdit{NewEmail: "csd9@csd.uoc.gr"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "csd9@csd.uoc.gr" || got.Domain != "csd.uoc.gr" || got.UserID == nil || *got.UserID != "u1" {
		t.Fatalf("updated record = %+v", got)
	}
	if _, err := s.WhoIs(ctx, "th1@edu.hmu.gr"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old address is still recorded: %v", err)
	}
	m, err := s.GetMember(ctx, "u1")
	if err != nil || m.VerifiedEmail == nil || *m.VerifiedEmail != "csd9@csd.uoc.gr" {
		t.Fatalf("member did not follow the corrected address: %+v, %v", m, err)
	}
	if use, _ := s.CheckEmail(ctx, "th1@edu.hmu.gr"); use.InUse {
		t.Error("the mistyped address is still spoken for")
	}
}

func TestUpdateVerifiedEmailRefusesATakenAddress(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.AddVerifiedEmail(ctx, "a@x.gr", nil, SourceModerator); err != nil {
		t.Fatal(err)
	}
	if err := s.AddVerifiedEmail(ctx, "b@x.gr", nil, SourceModerator); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateVerifiedEmail(ctx, "a@x.gr", EmailEdit{NewEmail: "B@x.gr"}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("rename onto an existing address = %v, want ErrEmailTaken", err)
	}
	if _, err := s.WhoIs(ctx, "a@x.gr"); err != nil {
		t.Fatalf("a failed rename lost the original: %v", err)
	}
	if _, err := s.UpdateVerifiedEmail(ctx, "nobody@x.gr", EmailEdit{Unlink: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of an unknown address = %v, want ErrNotFound", err)
	}
}

func TestUpdateVerifiedEmailMovesAndDropsTheOwner(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	verifiedMember(t, s, "u1", "th1@edu.hmu.gr")

	other := "u2"
	got, err := s.UpdateVerifiedEmail(ctx, "th1@edu.hmu.gr", EmailEdit{Owner: &other})
	if err != nil || got.UserID == nil || *got.UserID != "u2" {
		t.Fatalf("reassigned record = %+v, %v", got, err)
	}
	if m, _ := s.GetMember(ctx, "u1"); m.VerifiedEmail != nil {
		t.Errorf("the former owner still points at the address: %v", *m.VerifiedEmail)
	}

	got, err = s.UpdateVerifiedEmail(ctx, "th1@edu.hmu.gr", EmailEdit{Unlink: true})
	if err != nil || got.UserID != nil {
		t.Fatalf("unlinked record = %+v, %v", got, err)
	}
	if use, _ := s.CheckEmail(ctx, "th1@edu.hmu.gr"); !use.InUse || use.UserID != nil {
		t.Errorf("an unlinked address must stay spoken for with no owner: %+v", use)
	}
}

func TestLegacyHashReadAndUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	hash := LegacyHash("old@edu.hmu.gr")

	if _, err := s.GetLegacyHash(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash = %v, want ErrNotFound", err)
	}
	if ok, err := s.SetLegacyHashOwner(ctx, hash, nil); err != nil || ok {
		t.Fatalf("owner of a missing hash = %v, %v", ok, err)
	}
	if err := s.AddLegacyHash(ctx, " "+strings.ToUpper(hash)+" ", nil, SourceModerator); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetLegacyHash(ctx, hash)
	if err != nil || r.Hash != hash || r.UserID != nil || r.Source != SourceModerator {
		t.Fatalf("stored hash = %+v, %v", r, err)
	}

	owner := "u7"
	if ok, err := s.SetLegacyHashOwner(ctx, hash, &owner); err != nil || !ok {
		t.Fatalf("bind owner = %v, %v", ok, err)
	}
	mine, err := s.LegacyHashesForUser(ctx, "u7")
	if err != nil || len(mine) != 1 || mine[0].Hash != hash {
		t.Fatalf("hashes for u7 = %+v, %v", mine, err)
	}
	if ok, _ := s.SetLegacyHashOwner(ctx, hash, nil); !ok {
		t.Fatal("unbind owner failed")
	}
	if mine, _ := s.LegacyHashesForUser(ctx, "u7"); len(mine) != 0 {
		t.Fatalf("hash still bound after unbinding: %+v", mine)
	}
	if use, _ := s.CheckEmail(ctx, "old@edu.hmu.gr"); !use.InUse || !use.Legacy {
		t.Errorf("an unowned hash must still block its address: %+v", use)
	}
}
