package events

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClassifyEmail(t *testing.T) {
	cases := []struct {
		email string
		role  string
		ok    bool
	}{
		{"th123456@edu.hmu.gr", hmuRoleID, true},
		{"csd4321@csd.uoc.gr", csdRoleID, true},
		{"th123@edu.hmu.gr", hmuRoleID, true},
		{"random@gmail.com", "", false},
		{"th123@edu.hmu.gr.evil.com", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		role, ok := classifyEmail(c.email)
		if ok != c.ok || role != c.role {
			t.Errorf("classifyEmail(%q) = (%q,%v), want (%q,%v)", c.email, role, ok, c.role, c.ok)
		}
	}
}

func TestUserIDFromChannelName(t *testing.T) {
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"register-123456789", "123456789", true},
		{"register-", "", false},
		{"register-abc", "", false},
		{"general", "", false},
		{"register-overflow-2", "", false},
	}
	for _, c := range cases {
		id, ok := userIDFromChannelName(c.name)
		if ok != c.ok || id != c.id {
			t.Errorf("userIDFromChannelName(%q) = (%q,%v), want (%q,%v)", c.name, id, ok, c.id, c.ok)
		}
	}
}

func TestGenerateCodeFormat(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := generateCode()
		if err != nil {
			t.Fatalf("generateCode error: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("code %q length %d, want 6", code, len(code))
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("code %q has non-digit", code)
			}
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate left a short string as %q", got)
	}

	long := strings.Repeat("αβγ", 1000) // two bytes per rune
	for _, limit := range []int{1900, 1901, 50} {
		got := truncate(long, limit)
		if len(got) > limit {
			t.Errorf("limit %d: got %d bytes", limit, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("limit %d: result is not valid UTF-8", limit)
		}
		if !strings.HasSuffix(got, "(συντομεύτηκε)") {
			t.Errorf("limit %d: missing truncation marker", limit)
		}
	}
}

func TestQuoteNeutralisesCodeFences(t *testing.T) {
	if got := quote("a ```b``` c"); strings.Contains(got, "`") {
		t.Errorf("quote left a backtick in %q", got)
	}
}
