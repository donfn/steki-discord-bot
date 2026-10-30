package events

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestRateGatesLimitPerKey(t *testing.T) {
	var g rateGates
	for i := 0; i < 3; i++ {
		if !g.allow("a", 3, time.Hour) {
			t.Fatalf("call %d refused", i)
		}
	}
	if g.allow("a", 3, time.Hour) {
		t.Error("fourth call allowed")
	}
	if !g.allow("b", 3, time.Hour) {
		t.Error("other key limited")
	}
}

func TestIdentityShowsHandleAndStripsBidi(t *testing.T) {
	u := &discordgo.User{Username: "real", GlobalName: "Prof\u202e Smith"}
	if got := identity(u); got != "Prof Smith (@real)" {
		t.Fatalf("identity = %q", got)
	}
}

func TestRateGatesClear(t *testing.T) {
	var g rateGates
	g.allow("a", 1, time.Hour)
	g.allow("b", 1, time.Hour)
	g.clear("a")
	if !g.allow("a", 1, time.Hour) {
		t.Fatal("cleared key still refused")
	}
	if g.allow("b", 1, time.Hour) {
		t.Fatal("clear touched another key")
	}
}
