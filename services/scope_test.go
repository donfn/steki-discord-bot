package services

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// deps lists the module's own packages a command is built from.
func deps(t *testing.T, cmd string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", "steki-bot/cmd/"+cmd).Output()
	if err != nil {
		t.Fatal(err)
	}
	var own []string
	for _, line := range strings.Fields(string(out)) {
		if p, ok := strings.CutPrefix(line, "steki-bot/"); ok {
			own = append(own, p)
		}
	}
	return own
}

// TestServicesStayInTheirLane is what makes the split mean something: code a
// service does not link cannot run in it, however it is attacked.
func TestServicesStayInTheirLane(t *testing.T) {
	forbidden := map[string][]string{
		// Holds members' addresses and the mail key: no pages, no file handling.
		"registration": {"verify", "mcp", "materials", "usernotes", "shield", "services/moderation", "services/web"},
		// Reads every message: no pages, no files, no mail.
		"moderation": {"verify", "mcp", "materials", "usernotes", "shield", "mail", "services/registration", "services/web"},
		// Faces the internet: no mail, nor other services' logic.
		"web": {"mail", "services/registration", "services/moderation"},
	}
	for cmd, banned := range forbidden {
		have := deps(t, cmd)
		for _, p := range banned {
			if slices.Contains(have, p) {
				t.Errorf("%s links %s", cmd, p)
			}
		}
	}
}

// TestWatchPatternsCoverEveryDependency keeps a change to a package from silently
// not redeploying a service built from it.
func TestWatchPatternsCoverEveryDependency(t *testing.T) {
	for _, cmd := range []string{"registration", "moderation", "web"} {
		raw, err := os.ReadFile("../cmd/" + cmd + "/railway.toml")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range deps(t, cmd) {
			if !strings.Contains(string(raw), `"`+p+`/**"`) {
				t.Errorf("%s's watch patterns miss %s", cmd, p)
			}
		}
	}
}

// TestSecretsStayWithTheirService reads the configuration loaders: a variable
// appearing in the wrong loader would hand that container the secret.
func TestSecretsStayWithTheirService(t *testing.T) {
	raw, err := os.ReadFile("../config/config.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	section := func(fn string) string {
		i := strings.Index(src, "func "+fn+"()")
		if i < 0 {
			t.Fatalf("no %s", fn)
		}
		rest := src[i:]
		if j := strings.Index(rest[1:], "\nfunc "); j > 0 {
			return rest[:j]
		}
		return rest
	}
	onlyIn := map[string]string{
		"RESEND_API_KEY": "LoadRegistration", "JWT_SECRET": "LoadWeb", "RAILWAY_TOKEN": "LoadWeb",
		"DISCORD_CLIENT_SECRET": "LoadWeb", "GOOGLE_AI_TOKEN": "LoadWeb", "BLASTDOOR_URL": "LoadWeb",
	}
	for v, owner := range onlyIn {
		for _, fn := range []string{"LoadRegistration", "LoadModeration", "LoadWeb"} {
			if got := strings.Contains(section(fn), `"`+v+`"`); got != (fn == owner) {
				t.Errorf("%s in %s: %v", v, fn, got)
			}
		}
	}
}
