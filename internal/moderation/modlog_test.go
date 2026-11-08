package moderation

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("short"); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short message split into %q", got)
	}

	// Greek letters are two bytes each, so a naive byte cut would land
	// mid-rune on every chunk boundary.
	long := strings.Repeat("αβγ", 1500)
	chunks := splitMessage(long)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	if strings.Join(chunks, "") != long {
		t.Fatal("chunks do not reassemble to the original")
	}
	for i, c := range chunks {
		if len(c) > discordMessageLimit {
			t.Errorf("chunk %d is %d bytes, over the limit", i, len(c))
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
	}
}
