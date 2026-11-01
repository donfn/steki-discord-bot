package web

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
)

func TestMayUpload(t *testing.T) {
	member := func(roles ...string) *discordgo.Member { return &discordgo.Member{Roles: roles} }
	for name, tc := range map[string]struct {
		m    *discordgo.Member
		want bool
	}{
		"verified member":  {member(bot.HMURoleID), true},
		"verified outside": {member(bot.OutsiderRoleID), true},
		"moderator":        {member(bot.ModRoleID), true},
		"staff":            {member(bot.StaffRoleID), true},
		"no roles":         {member(), false},
		"other role only":  {member("1234"), false},
		"no member":        {nil, false},
	} {
		if got := mayUpload(tc.m); got != tc.want {
			t.Errorf("%s: mayUpload = %v, want %v", name, got, tc.want)
		}
	}
}
