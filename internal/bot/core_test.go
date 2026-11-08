package bot

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/config"
)

func TestGuardRecoversPanics(t *testing.T) {
	h := &Core{Log: discardLogger()}
	fn := Guard(h, "test", func(*discordgo.Session, *discordgo.Ready) { panic("boom") })
	fn(nil, &discordgo.Ready{}) // must not propagate
}

func TestIsHomeGuild(t *testing.T) {
	home := &discordgo.Guild{ID: "1", Channels: []*discordgo.Channel{
		{ID: "general"}, {ID: config.RegisterCategoryID},
	}}
	other := &discordgo.Guild{ID: "2", Channels: []*discordgo.Channel{{ID: "general"}}}

	if !IsHomeGuild(home) {
		t.Error("the guild owning the register category was not recognised")
	}
	if IsHomeGuild(other) || IsHomeGuild(nil) {
		t.Error("a guild without the register category was treated as home")
	}
}

func TestInHomeGuild(t *testing.T) {
	var h Core
	if h.InHomeGuild("") || h.InHomeGuild("1") {
		t.Error("no event may count as home before the home guild is known")
	}
	h.SetGuild("1")
	if !h.InHomeGuild("1") || h.InHomeGuild("2") || h.InHomeGuild("") {
		t.Error("only the stored guild may count as home")
	}
}

// A reconnect brings the guild again, and must not repeat what starting did.
func TestOnHomeRunsOnce(t *testing.T) {
	c := NewCore(discardLogger(), nil, nil)
	t.Cleanup(c.Stop)
	runs := 0
	c.OnHome = func(*discordgo.Session) { runs++ }
	home := &discordgo.GuildCreate{Guild: &discordgo.Guild{
		ID: "g1", Channels: []*discordgo.Channel{{ID: config.RegisterCategoryID}},
	}}
	c.onGuildCreate(nil, home)
	c.onGuildCreate(nil, home)
	c.onResumed(nil, &discordgo.Resumed{})
	if runs != 1 {
		t.Errorf("OnHome ran %d times, want once", runs)
	}
	if c.GuildID() != "g1" {
		t.Errorf("guild = %q", c.GuildID())
	}
}
