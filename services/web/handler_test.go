package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
)

// TestRegisterHandlersAreAccepted guards the generic panic wrapper: discordgo
// picks a handler by its exact function type and silently drops one it does
// not recognise, logging "Invalid handler type" instead of failing.
func TestRegisterHandlersAreAccepted(t *testing.T) {
	var logged []string
	prev := discordgo.Logger
	discordgo.Logger = func(_ int, _ int, format string, a ...any) {
		logged = append(logged, fmt.Sprintf(format, a...))
	}
	t.Cleanup(func() { discordgo.Logger = prev })

	s, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Core: bot.NewCore(discardLogger(), nil, nil)}
	h.Core.Register(s)
	s.AddHandler(bot.Guard(h.Core, "message_create", h.onMessage))
	s.AddHandler(bot.Guard(h.Core, "message_update", h.onMessageUpdate))
	s.AddHandler(bot.Guard(h.Core, "message_delete", h.onMessageDelete))
	s.AddHandler(bot.Guard(h.Core, "message_delete_bulk", h.onMessageDeleteBulk))
	s.AddHandler(bot.Guard(h.Core, "raw_event", h.onRawEvent))
	s.AddHandler(bot.Guard(h.Core, "interaction_create", h.onInteractionCreate))

	for _, line := range logged {
		if strings.Contains(line, "Invalid handler type") {
			t.Fatalf("discordgo rejected a handler: %s", line)
		}
	}
}
