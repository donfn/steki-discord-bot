package events

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestIsGhost(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cutoff := ghostCutoff(now)
	if want := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC); !cutoff.Equal(want) {
		t.Fatalf("cutoff = %v, want %v", cutoff, want)
	}
	ago := func(days int) time.Time { return now.AddDate(0, 0, -days) }
	at := func(days int) *time.Time { v := ago(days); return &v }

	cases := []struct {
		name   string
		joined time.Time
		last   *time.Time
		want   bool
	}{
		{"never posted, joined long ago", ago(400), nil, true},
		{"never posted, joined last week", ago(7), nil, false},
		{"posted yesterday", ago(400), at(1), false},
		{"posted just inside the window", ago(400), at(89), false},
		{"last post before the window", ago(400), at(120), true},
		{"last post before the window, rejoined recently", ago(10), at(120), false},
	}
	for _, c := range cases {
		if got := isGhost(c.joined, c.last, cutoff); got != c.want {
			t.Errorf("%s: isGhost = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCountsAsActivity(t *testing.T) {
	user := &discordgo.User{ID: "1"}
	cases := []struct {
		name string
		m    *discordgo.Message
		want bool
	}{
		{"plain message", &discordgo.Message{Author: user, Type: discordgo.MessageTypeDefault}, true},
		{"reply", &discordgo.Message{Author: user, Type: discordgo.MessageTypeReply}, true},
		{"join notice", &discordgo.Message{Author: user, Type: discordgo.MessageTypeGuildMemberJoin}, false},
		{"bot", &discordgo.Message{Author: &discordgo.User{ID: "2", Bot: true}}, false},
		{"webhook", &discordgo.Message{Author: user, WebhookID: "9"}, false},
		{"no author", &discordgo.Message{}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := countsAsActivity(c.m); got != c.want {
			t.Errorf("%s: countsAsActivity = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestJoinedAtFallsBackToNow(t *testing.T) {
	now := time.Now()
	if got := joinedAt(&discordgo.Member{}, now); !got.Equal(now) {
		t.Errorf("missing join time = %v, want now", got)
	}
	j := now.Add(-time.Hour)
	if got := joinedAt(&discordgo.Member{JoinedAt: j}, now); !got.Equal(j) {
		t.Errorf("join time = %v, want %v", got, j)
	}
}
