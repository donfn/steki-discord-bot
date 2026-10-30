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
	ResendAPIKey string

	// DatabasePath is the SQLite file. ImportDatabaseURL, when set, is a
	// Postgres database whose contents are copied into it once, on the first
	// boot that finds the file empty.
	DatabasePath      string
	ImportDatabaseURL string

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

	// Bucket is the S3-compatible bucket PDFs are backed up to. The material
	// backup, the sticky messages and the material page are all off unless
	// every field is set.
	Bucket Bucket

	// DiscordClientID and DiscordClientSecret are the Discord OAuth2
	// application's credentials, which the MCP server logs members in with.
	// The MCP server is off unless both are set (and the bucket is).
	DiscordClientID     string
	DiscordClientSecret string

	// GoogleAIToken is a Google AI Studio API key. With it, uploaded images
	// are checked by a Gemini model and the ones that are not course material
	// or show people wait for an admin. GoogleAIModel overrides the model.
	GoogleAIToken string
	GoogleAIModel string

	// PostHogKey is the PostHog project API key. With it, the gateway events
	// and the pages the bot serves are reported to PostHog from the server;
	// without it analytics is off. PostHogHost is the ingestion endpoint.
	PostHogKey  string
	PostHogHost string
}

// MCPEnabled reports whether the MCP server has the Discord login it needs.
func (c *Config) MCPEnabled() bool {
	return c.DiscordClientID != "" && c.DiscordClientSecret != ""
}

// Bucket holds the S3 connection details of the backup bucket. On Railway
// they are references to the steki-backups bucket's own variables.
type Bucket struct {
	Endpoint, Region, Name, AccessKeyID, SecretAccessKey string
}

// Enabled reports whether the bucket is fully configured.
func (b Bucket) Enabled() bool {
	return b.Endpoint != "" && b.Name != "" && b.AccessKeyID != "" && b.SecretAccessKey != ""
}

// Load reads configuration from the environment, falling back to a .env file
// when one is present. It returns an error naming every missing required
// variable at once, so a misconfigured deploy fails on boot with a complete
// list rather than one variable at a time.
func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		DiscordToken: os.Getenv("DISCORD_TOKEN"),
		ResendAPIKey: os.Getenv("RESEND_API_KEY"),

		// The Postgres URL the bot used to require is now only a source to
		// copy from; the variable keeps its name so the existing Railway
		// reference (${{Postgres.DATABASE_URL}}) can stay in place for that.
		ImportDatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),
		EmailFrom:         envOr("EMAIL_FROM", "Steki <steki@poiw.org>"),
		LogLevel:          parseLevel(os.Getenv("LOG_LEVEL")),
		Port:              envOr("PORT", "8080"),
		JWTSecret:         strings.TrimSpace(os.Getenv("JWT_SECRET")),
		PublicURL:         publicURL(),

		RailwayToken:     strings.TrimSpace(os.Getenv("RAILWAY_TOKEN")),
		RailwayServiceID: strings.TrimSpace(os.Getenv("RAILWAY_SERVICE_ID")),

		DiscordClientID:     strings.TrimSpace(os.Getenv("DISCORD_CLIENT_ID")),
		DiscordClientSecret: strings.TrimSpace(os.Getenv("DISCORD_CLIENT_SECRET")),

		GoogleAIToken: strings.TrimSpace(os.Getenv("GOOGLE_AI_TOKEN")),
		GoogleAIModel: strings.TrimSpace(os.Getenv("GOOGLE_AI_MODEL")),

		PostHogKey:  strings.TrimSpace(os.Getenv("POSTHOG_API_KEY")),
		PostHogHost: envOr("POSTHOG_HOST", "https://eu.i.posthog.com"),

		Bucket: Bucket{
			Endpoint:        strings.TrimSpace(os.Getenv("BUCKET_ENDPOINT")),
			Region:          envOr("BUCKET_REGION", "auto"),
			Name:            strings.TrimSpace(os.Getenv("BUCKET_NAME")),
			AccessKeyID:     strings.TrimSpace(os.Getenv("BUCKET_ACCESS_KEY_ID")),
			SecretAccessKey: strings.TrimSpace(os.Getenv("BUCKET_SECRET_ACCESS_KEY")),
		},
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
	if path, err := databasePath(); err != nil {
		return nil, err
	} else {
		cfg.DatabasePath = path
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

// databasePath is where the SQLite file lives: DATABASE_PATH when set, else
// steki.db inside the Railway volume, else a file in the working directory.
//
// On Railway without a volume the container's disk is wiped by every deploy,
// so a database there would silently vanish; that is refused rather than
// allowed to happen.
func databasePath() (string, error) {
	if v := strings.TrimSpace(os.Getenv("DATABASE_PATH")); v != "" {
		return v, nil
	}
	if m := strings.TrimSpace(os.Getenv("RAILWAY_VOLUME_MOUNT_PATH")); m != "" {
		return strings.TrimRight(m, "/") + "/steki.db", nil
	}
	if os.Getenv("RAILWAY_ENVIRONMENT_ID") != "" {
		return "", fmt.Errorf("no volume is attached to this Railway service, and the database " +
			"would be erased on every deploy: attach a volume, or set DATABASE_PATH to a file on one")
	}
	return "steki.db", nil
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
