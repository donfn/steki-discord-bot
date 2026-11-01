package registration

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
)

func TestClassifyEmail(t *testing.T) {
	cases := []struct {
		email string
		role  string
		ok    bool
	}{
		{"th123456@edu.hmu.gr", bot.HMURoleID, true},
		{"csd4321@csd.uoc.gr", bot.OutsiderRoleID, true},
		{"tp12345@edu.hmu.gr", bot.OutsiderRoleID, true},
		{"tp@edu.hmu.gr", "", false},
		{"tp123@edu.hmu.gr.evil.com", "", false},
		{"th123@edu.hmu.gr", bot.HMURoleID, true},
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

func TestIdentityShowsHandleAndStripsBidi(t *testing.T) {
	u := &discordgo.User{Username: "real", GlobalName: "Prof\u202e Smith"}
	if got := identity(u); got != "Prof Smith (@real)" {
		t.Fatalf("identity = %q", got)
	}
}
