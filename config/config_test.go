package config

import (
	"strings"
	"testing"
)

func setRequired(t *testing.T, secret string) {
	t.Helper()
	t.Setenv("DISCORD_TOKEN", "token")
	t.Setenv("RESEND_API_KEY", "key")
	t.Setenv("PUBLIC_URL", "https://steki.example")
	t.Setenv("DATABASE_PATH", t.TempDir()+"/steki.db")
	t.Setenv("JWT_SECRET", secret)
}

// Every link and token is signed from JWT_SECRET, so a short one is refused
// at start-up rather than warned about.
func TestShortJWTSecretIsRefused(t *testing.T) {
	for _, secret := range []string{"secret", strings.Repeat("a", MinSecretLen-1)} {
		setRequired(t, secret)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
			t.Errorf("Load accepted a %d character secret: %v", len(secret), err)
		}
	}
	setRequired(t, strings.Repeat("a", MinSecretLen))
	if _, err := Load(); err != nil {
		t.Errorf("Load refused a %d character secret: %v", MinSecretLen, err)
	}
}
