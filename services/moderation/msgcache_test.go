package moderation

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func msg(id, author, content string, bot bool) *discordgo.Message {
	return &discordgo.Message{ID: id, Content: content, Author: &discordgo.User{ID: author, Bot: bot}}
}

func TestMsgCacheEditReturnsPreviousContent(t *testing.T) {
	c := newMsgCache(4)
	c.put(msg("1", "u1", "hello", false))

	before, ok := c.edit("1", "hello world")
	if !ok || before.content != "hello" || before.authorID != "u1" {
		t.Fatalf("edit = %+v, %v", before, ok)
	}
	again, _ := c.edit("1", "hello world!")
	if again.content != "hello world" {
		t.Fatalf("second edit saw %q, want the first edit's text", again.content)
	}
	if _, ok := c.edit("unknown", "x"); ok {
		t.Fatal("edit of an uncached message reported a hit")
	}
}

func TestMsgCacheTakeRemoves(t *testing.T) {
	c := newMsgCache(4)
	c.put(msg("1", "u1", "bye", true))
	m, ok := c.take("1")
	if !ok || m.content != "bye" || !m.bot {
		t.Fatalf("take = %+v, %v", m, ok)
	}
	if _, ok := c.take("1"); ok {
		t.Fatal("message still cached after take")
	}
}

func TestMsgCacheEvictsOldestAndStaysBounded(t *testing.T) {
	c := newMsgCache(3)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		c.put(msg(id, "u", "m"+id, false))
	}
	if len(c.at) != 3 {
		t.Fatalf("index holds %d entries, want 3", len(c.at))
	}
	for _, gone := range []string{"1", "2"} {
		if _, ok := c.take(gone); ok {
			t.Errorf("message %s survived eviction", gone)
		}
	}
	if m, ok := c.take("5"); !ok || m.content != "m5" {
		t.Errorf("newest message missing: %+v, %v", m, ok)
	}
}

func TestClipKeepsUTF8Whole(t *testing.T) {
	s := strings.Repeat("ω", 10) // two bytes each
	got := clip(s, 5)
	if got != strings.Repeat("ω", 2) {
		t.Fatalf("clip = %q", got)
	}
	if clip("short", 100) != "short" {
		t.Fatal("clip changed a short string")
	}
}
