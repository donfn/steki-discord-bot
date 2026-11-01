package bot

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

func TestUserIDFromChannelName(t *testing.T) {
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"register-123456789", "123456789", true},
		{"register-", "", false},
		{"register-abc", "", false},
		{"general", "", false},
		{"register-overflow-2", "", false},
	}
	for _, c := range cases {
		id, ok := UserIDFromChannelName(c.name)
		if ok != c.ok || id != c.id {
			t.Errorf("UserIDFromChannelName(%q) = (%q,%v), want (%q,%v)", c.name, id, ok, c.id, c.ok)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 100); got != "short" {
		t.Errorf("truncate left a short string as %q", got)
	}

	long := strings.Repeat("αβγ", 1000) // two bytes per rune
	for _, limit := range []int{1900, 1901, 50} {
		got := Truncate(long, limit)
		if len(got) > limit {
			t.Errorf("limit %d: got %d bytes", limit, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("limit %d: result is not valid UTF-8", limit)
		}
		if !strings.HasSuffix(got, "(συντομεύτηκε)") {
			t.Errorf("limit %d: missing truncation marker", limit)
		}
	}
}

func TestQuoteNeutralisesCodeFences(t *testing.T) {
	if got := Quote("a ```b``` c"); strings.Contains(got, "`") {
		t.Errorf("quote left a backtick in %q", got)
	}
}

func TestCommandPath(t *testing.T) {
	withSub := discordgo.ApplicationCommandInteractionData{
		Name: "reg",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: "stuck", Type: discordgo.ApplicationCommandOptionSubCommand},
		},
	}
	if got := CommandPath(withSub); got != "reg stuck" {
		t.Errorf("commandPath = %q, want %q", got, "reg stuck")
	}

	bare := discordgo.ApplicationCommandInteractionData{Name: "reg"}
	if got := CommandPath(bare); got != "reg" {
		t.Errorf("commandPath without subcommand = %q, want %q", got, "reg")
	}
}

func TestCleanLine(t *testing.T) {
	// Invisible characters are written as escapes, so the source shows them.
	cases := []struct{ name, in, want string }{
		{"plain", "Θέματα Σεπτεμβρίου 2024", "Θέματα Σεπτεμβρίου 2024"},
		{"whitespace collapsed", "  Σημειώσεις \t\n  Ανάλυση  ", "Σημειώσεις Ανάλυση"},
		{"zero-width dropped", "Θέ\u200bματα\u2060", "Θέματα"},
		{"bidi override dropped", "fdp.\u202etxt", "fdp.txt"},
		{"bidi isolate dropped", "\u2066a\u2069", "a"},
		{"control dropped", "a\x00b\x07c", "abc"},
		{"empty", " \u200b ", ""},
	}
	for _, c := range cases {
		if got := CleanLine(c.in); got != c.want {
			t.Errorf("%s: CleanLine(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
