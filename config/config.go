// Package config loads and validates the bot's runtime configuration.
//
// Everything that is fixed for the Steki guild (channel, category and role IDs)
// lives as a constant here rather than as an environment variable: those IDs
// never change, and keeping them out of the environment is what makes the
// deployment configuration small enough to read at a glance.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// Discord IDs for the Steki guild. These are compile-time constants on purpose.
// The guild ID itself is deliberately absent: it is read from the gateway at
// runtime, so the bot never has to be told which guild it is in.
const (
	LogChannelID       = "939180081857839174"
	WelcomeChannelID   = "886981920108478526"
	SupportChannelID   = "939177847933780018"
	RegisterCategoryID = "890021609631531009"
)

// Config holds everything the bot needs to run.
type Config struct {
	DiscordToken string
	DatabaseURL  string
	ResendAPIKey string

	EmailFrom    string
	URLBlacklist []string
	LogLevel     slog.Level
	Port         string

	// JWTSecret signs the registration and media guard links and keys their
	// captcha. PublicURL is where those links point. Registration depends on
	// both, so both are required.
	JWTSecret string
	PublicURL string

	// RailwayToken is a project token. When set, the bot turns on Railway's
	// Under Attack Mode for its own service if it sees a flood. Optional.
	RailwayToken     string
	RailwayServiceID string
}

// Load reads configuration from the environment, falling back to a .env file
// when one is present. It returns an error naming every missing required
// variable at once, so a misconfigured deploy fails on boot with a complete
// list rather than one variable at a time.
func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		DiscordToken: os.Getenv("DISCORD_TOKEN"),
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		ResendAPIKey: os.Getenv("RESEND_API_KEY"),
		EmailFrom:    envOr("EMAIL_FROM", "Steki <steki@poiw.org>"),
		LogLevel:     parseLevel(os.Getenv("LOG_LEVEL")),
		Port:         envOr("PORT", "8080"),
		JWTSecret:    strings.TrimSpace(os.Getenv("JWT_SECRET")),
		PublicURL:    publicURL(),

		RailwayToken:     strings.TrimSpace(os.Getenv("RAILWAY_TOKEN")),
		RailwayServiceID: strings.TrimSpace(os.Getenv("RAILWAY_SERVICE_ID")),
	}

	for _, entry := range strings.Split(os.Getenv("URL_BLACKLIST"), ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			cfg.URLBlacklist = append(cfg.URLBlacklist, strings.ToLower(entry))
		}
	}

	var missing []string
	if cfg.DiscordToken == "" {
		missing = append(missing, "DISCORD_TOKEN")
	}
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.ResendAPIKey == "" {
		missing = append(missing, "RESEND_API_KEY")
	}
	if cfg.JWTSecret == "" {
		missing = append(missing, "JWT_SECRET")
	}
	if cfg.PublicURL == "" {
		missing = append(missing, "PUBLIC_URL (or a Railway public domain)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
		return nil, fmt.Errorf("PUBLIC_URL must be an https origin (got %q)", cfg.PublicURL)
	}

	return cfg, nil
}

// publicURL is PUBLIC_URL when set, otherwise the domain Railway injects for
// a service with a public domain.
func publicURL() string {
	if v := strings.TrimSpace(os.Getenv("PUBLIC_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	if d := strings.TrimSpace(os.Getenv("RAILWAY_PUBLIC_DOMAIN")); d != "" {
		return "https://" + d
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
