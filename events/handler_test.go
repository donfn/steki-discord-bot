package events

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
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
	(&Handler{}).Register(s)

	for _, line := range logged {
		if strings.Contains(line, "Invalid handler type") {
			t.Fatalf("discordgo rejected a handler: %s", line)
		}
	}
}

func TestGuardRecoversPanics(t *testing.T) {
	h := &Handler{log: discardLogger()}
	fn := guard(h, "test", func(*discordgo.Session, *discordgo.Ready) { panic("boom") })
	fn(nil, &discordgo.Ready{}) // must not propagate
}

func TestIsHomeGuild(t *testing.T) {
	home := &discordgo.Guild{ID: "1", Channels: []*discordgo.Channel{
		{ID: "general"}, {ID: config.RegisterCategoryID},
	}}
	other := &discordgo.Guild{ID: "2", Channels: []*discordgo.Channel{{ID: "general"}}}

	if !isHomeGuild(home) {
		t.Error("the guild owning the register category was not recognised")
	}
	if isHomeGuild(other) || isHomeGuild(nil) {
		t.Error("a guild without the register category was treated as home")
	}
}

func TestInHomeGuild(t *testing.T) {
	var h Handler
	if h.inHomeGuild("") || h.inHomeGuild("1") {
		t.Error("no event may count as home before the home guild is known")
	}
	h.guild.Store("1")
	if !h.inHomeGuild("1") || h.inHomeGuild("2") || h.inHomeGuild("") {
		t.Error("only the stored guild may count as home")
	}
}
