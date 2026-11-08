package dc

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestOverflowIndex(t *testing.T) {
	cases := []struct {
		name string
		n    int
		ok   bool
	}{
		{"register-overflow-2", 2, true},
		{"register-overflow-10", 10, true},
		{"register-overflow-1", 0, false}, // overflow starts at 2
		{"register-overflow-x", 0, false},
		{"something-else", 0, false},
	}
	for _, c := range cases {
		n, ok := overflowIndex(c.name)
		if ok != c.ok || n != c.n {
			t.Errorf("overflowIndex(%q) = (%d,%v), want (%d,%v)", c.name, n, ok, c.n, c.ok)
		}
	}
}

func TestChildCountsAndOrdering(t *testing.T) {
	channels := []*discordgo.Channel{
		{ID: "base", Type: discordgo.ChannelTypeGuildCategory},
		{ID: "of3", Name: "register-overflow-3", Type: discordgo.ChannelTypeGuildCategory},
		{ID: "of2", Name: "register-overflow-2", Type: discordgo.ChannelTypeGuildCategory},
		{ID: "c1", ParentID: "base"},
		{ID: "c2", ParentID: "base"},
		{ID: "c3", ParentID: "of2"},
	}
	counts := childCounts(channels)
	if counts["base"] != 2 || counts["of2"] != 1 || counts["of3"] != 0 {
		t.Fatalf("childCounts = %v", counts)
	}

	ordered := registerCategories(channels, "base")
	want := []string{"base", "of2", "of3"}
	if len(ordered) != len(want) {
		t.Fatalf("registerCategories len = %d, want %d", len(ordered), len(want))
	}
	for i, c := range ordered {
		if c.ID != want[i] {
			t.Errorf("position %d = %q, want %q", i, c.ID, want[i])
		}
	}
}

func TestNextOverflowIndex(t *testing.T) {
	cases := []struct {
		name     string
		channels []*discordgo.Channel
		want     int
	}{
		{"none yet", nil, 2},
		{"after 2", []*discordgo.Channel{
			{Name: "register-overflow-2", Type: discordgo.ChannelTypeGuildCategory},
		}, 3},
		{"gap is not reused", []*discordgo.Channel{
			{Name: "register-overflow-4", Type: discordgo.ChannelTypeGuildCategory},
			{Name: "register-overflow-2", Type: discordgo.ChannelTypeGuildCategory},
		}, 5},
		{"text channel with the name is ignored", []*discordgo.Channel{
			{Name: "register-overflow-9", Type: discordgo.ChannelTypeGuildText},
		}, 2},
	}
	for _, c := range cases {
		if got := nextOverflowIndex(c.channels); got != c.want {
			t.Errorf("%s: nextOverflowIndex = %d, want %d", c.name, got, c.want)
		}
	}
}
