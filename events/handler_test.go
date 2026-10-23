package events

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
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
