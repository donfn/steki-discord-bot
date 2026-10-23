package store

import "testing"

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
