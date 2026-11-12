package moderation

import (
	"strings"
	"testing"
	"time"
)

func TestStatusFrames(t *testing.T) {
	var total int64
	for _, a := range animations {
		total += int64(len(a)) + 1
	}
	seen := map[string]bool{}
	var infos int
	for n := int64(0); n < total*2; n++ {
		f := statusFrame(n, time.Hour, 3, 7)
		if f == "" || len([]rune(f)) > 128 {
			t.Fatalf("frame %d = %q", n, f)
		}
		if strings.HasPrefix(f, "Uptime") || strings.Contains(f, "μηνύματα") {
			infos++
		}
		seen[f] = true
	}
	if infos != 2*len(animations) {
		t.Errorf("info frames = %d, want one after each animation, twice over", infos)
	}
	if len(seen) < 40 {
		t.Errorf("only %d distinct frames", len(seen))
	}
	if statusFrame(0, 0, 0, 0) != animations[0][0] || statusFrame(total, 0, 0, 0) != animations[0][0] {
		t.Error("the cycle does not wrap to the first frame")
	}
}
