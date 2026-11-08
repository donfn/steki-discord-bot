package services

import (
	"testing"
	"unicode/utf8"

	"steki-bot/internal/bot"
	"steki-bot/services/moderation"
	"steki-bot/services/registration"
	"steki-bot/services/web"

	"github.com/bwmarrin/discordgo"
)

// TestEveryCommandHasOneOwner keeps the command list and the services' routes in
// step: a subcommand Discord offers that no service owns gets no answer, one that
// two own gets two, and the second fails.
func TestEveryCommandHasOneOwner(t *testing.T) {
	owners := map[string]int{}
	for _, routes := range [][]string{registration.Routes(), moderation.Routes(), web.Routes()} {
		for _, path := range routes {
			owners[path]++
		}
	}
	for _, cmd := range bot.Commands() {
		for _, sub := range cmd.Options {
			path := cmd.Name + " " + sub.Name
			if owners[path] != 1 {
				t.Errorf("%q is answered by %d services, want 1", path, owners[path])
			}
			delete(owners, path)
		}
	}
	for path := range owners {
		t.Errorf("%q is routed but no such command is defined", path)
	}
}

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
	for _, cmd := range bot.Commands() {
		if n := utf8.RuneCountInString(cmd.Description); n == 0 || n > 100 {
			t.Errorf("/%s: description is %d characters, want 1-100", cmd.Name, n)
		}
		check("/"+cmd.Name, cmd.Options)
	}
}

// TestEveryCommandIsStaffOnly keeps a new command from being offered to
// everyone: Discord shows a command to the members its default permission
// allows, and the on-demand passes (/reg reconcile, /materials stickies, ...)
// change many things at once.
func TestEveryCommandIsStaffOnly(t *testing.T) {
	for _, cmd := range bot.Commands() {
		if cmd.DefaultMemberPermissions == nil || *cmd.DefaultMemberPermissions != int64(discordgo.PermissionManageGuild) {
			t.Errorf("/%s is not limited to staff by default", cmd.Name)
		}
	}
}

// TestEveryTextOptionIsBounded keeps a free-text option from reaching the
// database or the audit log at Discord's limit of 6,000 characters. Discord
// also refuses the whole set of commands if a limit is out of range, which
// would leave the bot with none.
func TestEveryTextOptionIsBounded(t *testing.T) {
	var check func(path string, opts []*discordgo.ApplicationCommandOption)
	check = func(path string, opts []*discordgo.ApplicationCommandOption) {
		for _, o := range opts {
			where := path + " " + o.Name
			if o.Type == discordgo.ApplicationCommandOptionString && (o.MaxLength < 1 || o.MaxLength > 6000) {
				t.Errorf("%s: text option has max_length %d, want 1-6000; add it to bot.TextLimits", where, o.MaxLength)
			}
			check(where, o.Options)
		}
	}
	for _, cmd := range bot.Commands() {
		check("/"+cmd.Name, cmd.Options)
	}
}
