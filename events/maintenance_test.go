package events

import (
	"strconv"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/store"
)

// snowflakeAt builds a Discord ID whose embedded timestamp is t.
func snowflakeAt(t time.Time) string {
	const discordEpochMs = 1420070400000
	return strconv.FormatInt((t.UnixMilli()-discordEpochMs)<<22, 10)
}

func TestCreatedAfter(t *testing.T) {
	now := time.Now()
	if createdAfter(snowflakeAt(now.Add(-time.Hour)), now) {
		t.Error("an hour-old channel was treated as new")
	}
	if !createdAfter(snowflakeAt(now.Add(time.Minute)), now) {
		t.Error("a newer channel was not treated as new")
	}
	if !createdAfter("not-a-snowflake", now) {
		t.Error("an unparseable ID must count as new so it is left alone")
	}
}

func TestRegisterChannelFate(t *testing.T) {
	const (
		chA = "100"
		chB = "200"
	)
	unverified := &discordgo.Member{Roles: []string{"some-interest-role"}}
	verified := &discordgo.Member{Roles: []string{hmuRoleID}}
	regOn := func(id string) *store.Registration { return &store.Registration{ChannelID: &id} }
	bothLive := map[string]bool{chA: true, chB: true}

	cases := []struct {
		name   string
		reg    *store.Registration
		member *discordgo.Member
		live   map[string]bool
		want   channelFate
	}{
		{"registration points here", regOn(chA), unverified, bothLive, fateKeep},
		{"member left", nil, nil, bothLive, fateDelete},
		{"member already verified", regOn(chB), verified, bothLive, fateDelete},
		{"no registration yet", nil, unverified, bothLive, fateAdopt},
		{"registration lost its channel", &store.Registration{}, unverified, bothLive, fateAdopt},
		{"registration's channel is gone", regOn(chB), unverified, map[string]bool{chA: true}, fateAdopt},
		{"second channel for the same member", regOn(chB), unverified, bothLive, fateDeleteDuplicate},
	}
	for _, c := range cases {
		if got := registerChannelFate(chA, c.reg, c.member, c.live); got != c.want {
			t.Errorf("%s: fate = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestDuplicateChannelsSettle replays two reconciler passes over a member with
// two register channels: the first pass must leave one channel and the second
// pass must find nothing to do, rather than switching between them forever.
func TestDuplicateChannelsSettle(t *testing.T) {
	member := &discordgo.Member{}
	live := map[string]bool{"100": true, "200": true}
	var reg *store.Registration // no open row yet

	for pass := 1; pass <= 2; pass++ {
		var adopted, removed int
		for _, ch := range []string{"100", "200"} {
			if !live[ch] {
				continue
			}
			switch registerChannelFate(ch, reg, member, live) {
			case fateAdopt:
				id := ch
				reg = &store.Registration{ChannelID: &id}
				adopted++
			case fateDeleteDuplicate, fateDelete:
				delete(live, ch)
				removed++
			}
		}
		if pass == 1 && (adopted != 1 || removed != 1) {
			t.Fatalf("pass 1: adopted %d, removed %d; want 1 and 1", adopted, removed)
		}
		if pass == 2 && (adopted != 0 || removed != 0) {
			t.Fatalf("pass 2 still changing things: adopted %d, removed %d", adopted, removed)
		}
	}
}
