package config

import (
	"strings"
	"testing"
)

func setRequired(t *testing.T, secret string) {
	t.Helper()
	t.Setenv("DISCORD_TOKEN", "token")
	t.Setenv("PUBLIC_URL", "https://steki.example")
	t.Setenv("INTERNAL_SECRET", strings.Repeat("i", MinSecretLen))
	t.Setenv("BUCKET_ENDPOINT", "https://s3.example")
	t.Setenv("BUCKET_NAME", "b")
	t.Setenv("BUCKET_ACCESS_KEY_ID", "k")
	t.Setenv("BUCKET_SECRET_ACCESS_KEY", "s")
	t.Setenv("REGISTRATION_URL", "http://registration:8081")
	t.Setenv("MODERATION_URL", "http://moderation:8081")
	t.Setenv("WEB_URL", "http://web:8081")
	t.Setenv("JWT_SECRET", secret)
}

// Every link and token is signed from JWT_SECRET, so a short one is refused
// at start-up rather than warned about.
func TestShortJWTSecretIsRefused(t *testing.T) {
	for _, secret := range []string{"secret", strings.Repeat("a", MinSecretLen-1)} {
		setRequired(t, secret)
		if _, err := LoadWeb(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
			t.Errorf("Load accepted a %d character secret: %v", len(secret), err)
		}
	}
	setRequired(t, strings.Repeat("a", MinSecretLen))
	if _, err := LoadWeb(); err != nil {
		t.Errorf("Load refused a %d character secret: %v", MinSecretLen, err)
	}
}
