package events

import (
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// TestEveryCommandHasARoute keeps commandDefs and commandRoutes in step: a
// subcommand Discord offers with no handler answers "unknown command", and a
// handler no subcommand reaches is dead code.
func TestEveryCommandHasARoute(t *testing.T) {
	defined := map[string]bool{}
	for _, cmd := range commandDefs {
		if len(cmd.Options) == 0 {
			defined[cmd.Name] = true
			continue
		}
		for _, sub := range cmd.Options {
			defined[cmd.Name+" "+sub.Name] = true
		}
	}
	for path := range defined {
		if _, ok := commandRoutes[path]; !ok {
			t.Errorf("/%s is defined but has no handler", path)
		}
	}
	for path := range commandRoutes {
		if !defined[path] {
			t.Errorf("handler %q has no command definition", path)
		}
	}
}

// TestCommandDefinitionsFitDiscordLimits catches what Discord would reject when
// the whole set is overwritten at boot, which would leave no commands at all.
func TestCommandDefinitionsFitDiscordLimits(t *testing.T) {
	var check func(path string, opts []*discordgo.ApplicationCommandOption)
	check = func(path string, opts []*discordgo.ApplicationCommandOption) {
		seen := map[string]bool{}
		required := true
		for _, o := range opts {
			where := path + " " + o.Name
			if seen[o.Name] {
				t.Errorf("%s: duplicate option", where)
			}
			seen[o.Name] = true
			if n := utf8.RuneCountInString(o.Description); n == 0 || n > 100 {
				t.Errorf("%s: description is %d characters, want 1-100", where, n)
			}
			if o.Type != discordgo.ApplicationCommandOptionSubCommand {
				// Required options must all come before optional ones.
				if o.Required && !required {
					t.Errorf("%s: required option follows an optional one", where)
				}
				required = required && o.Required
			}
			check(where, o.Options)
		}
		if len(opts) > 25 {
			t.Errorf("%s: %d options, Discord allows 25", path, len(opts))
		}
	}
	for _, cmd := range commandDefs {
		if n := utf8.RuneCountInString(cmd.Description); n == 0 || n > 100 {
			t.Errorf("/%s: description is %d characters, want 1-100", cmd.Name, n)
		}
		check("/"+cmd.Name, cmd.Options)
	}
}
