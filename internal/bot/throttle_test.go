package bot

import (
	"testing"
	"time"
)

func TestRateGatesLimitPerKey(t *testing.T) {
	var g RateGates
	for i := 0; i < 3; i++ {
		if !g.Allow("a", 3, time.Hour) {
			t.Fatalf("call %d refused", i)
		}
	}
	if g.Allow("a", 3, time.Hour) {
		t.Error("fourth call allowed")
	}
	if !g.Allow("b", 3, time.Hour) {
		t.Error("other key limited")
	}
}

func TestRateGatesClear(t *testing.T) {
	var g RateGates
	g.Allow("a", 1, time.Hour)
	g.Allow("b", 1, time.Hour)
	g.Clear("a")
	if !g.Allow("a", 1, time.Hour) {
		t.Fatal("cleared key still refused")
	}
	if g.Allow("b", 1, time.Hour) {
		t.Fatal("clear touched another key")
	}
}
