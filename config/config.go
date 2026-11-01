// Package config loads and validates each service's runtime configuration. A
// service reads only the variables it needs, so a secret it does not hold cannot
// leak from it.
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
	"path/filepath"
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

// MinSecretLen is the shortest secret a service starts with.
const MinSecretLen = 32

// Bucket holds the S3 connection details of the bucket. On Railway they are
// references to the web service's variables, which in turn reference the bucket.
type Bucket struct {
	Endpoint, Region, Name, AccessKeyID, SecretAccessKey string
	// PathStyle is for servers without virtual-hosted buckets, such as a local
	// MinIO.
	PathStyle bool
}

// Enabled reports whether the bucket is fully configured.
func (b Bucket) Enabled() bool {
	return b.Endpoint != "" && b.Name != "" && b.AccessKeyID != "" && b.SecretAccessKey != ""
}

// Common is what every service needs.
type Common struct {
	DiscordToken string
	// InternalSecret signs requests between the services and the links the
	// registration and moderation services send.
	InternalSecret string
	// Bucket holds the databases.
	Bucket   Bucket
	LogLevel slog.Level
	Port     string

	// Deployment identifies this deployment to the databases' fencing.
	Deployment string
	// DataDir is the scratch directory the databases' working copies live in.
	DataDir string
	// CreateEmptyDB lets a fresh local setup start with empty databases.
	CreateEmptyDB bool

	PostHogKey  string
	PostHogHost string
}

// Registration configures the registration service.
type Registration struct {
	Common
	ResendAPIKey string
	EmailFrom    string
	PublicURL    string
	// ModerationURL and WebURL are the other services' internal APIs.
	ModerationURL string
	WebURL        string
}

// Moderation configures the moderation service.
type Moderation struct {
	Common
	URLBlacklist []string
	PublicURL    string
	// RegistrationURL and WebURL are the other services' internal APIs.
	RegistrationURL string
	WebURL          string
}

// Web configures the web service: the public pages, the MCP server and the
// course material.
type Web struct {
	Common
	// JWTSecret signs the material links, sessions and MCP tokens. No other
	// service holds it.
	JWTSecret string
	PublicURL string

	// LegacyDatabasePath is the old single database on the volume, split into
	// shards once. It is empty once the volume is detached.
	LegacyDatabasePath string

	// RailwayToken is a project token. When set, the bot writes the HTTP
	// shield's IP challenges and bans to its service's edge rules, and turns
	// on Railway's Under Attack Mode if it sees a flood. Optional. Railway
	// sets the service and environment IDs itself.
	RailwayToken         string
	RailwayServiceID     string
	RailwayEnvironmentID string

	// DiscordClientID and DiscordClientSecret are the Discord OAuth2
	// application's credentials, which the MCP server logs members in with.
	// The MCP server is off unless both are set.
	DiscordClientID     string
	DiscordClientSecret string

	// GoogleAIToken is a Google AI Studio API key. With it, uploaded images
	// are checked by a Gemini model and the ones that are not course material
	// or show people wait for an admin. GoogleAIModel overrides the model.
	GoogleAIToken string
	GoogleAIModel string

	// BlastdoorURL is the isolated service that decodes uploaded PNG, WebP and
	// HEIC images and compresses the files in the bucket.
	BlastdoorURL string

	// RegistrationURL and ModerationURL are the other services' internal APIs.
	RegistrationURL string
	ModerationURL   string
}

// MCPEnabled reports whether the MCP server has the Discord login it needs.
func (c *Web) MCPEnabled() bool {
	return c.DiscordClientID != "" && c.DiscordClientSecret != ""
}

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }

// loader gathers every missing variable, so a misconfigured deploy fails on boot
// with a complete list rather than one variable at a time.
type loader struct{ missing []string }

func (l *loader) need(key string) string {
	v := env(key)
	if v == "" {
		l.missing = append(l.missing, key)
	}
	return v
}

func (l *loader) err() error {
	if len(l.missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing required environment variables: %s", strings.Join(l.missing, ", "))
}

func (l *loader) common() Common {
	_ = godotenv.Load()
	deployment := env("RAILWAY_DEPLOYMENT_ID")
	if deployment == "" {
		deployment = fmt.Sprintf("local-%d", os.Getpid())
	}
	return Common{
		DiscordToken:   l.need("DISCORD_TOKEN"),
		InternalSecret: l.need("INTERNAL_SECRET"),
		Bucket: Bucket{
			Endpoint:        l.need("BUCKET_ENDPOINT"),
			Region:          envOr("BUCKET_REGION", "auto"),
			Name:            l.need("BUCKET_NAME"),
			AccessKeyID:     l.need("BUCKET_ACCESS_KEY_ID"),
			SecretAccessKey: l.need("BUCKET_SECRET_ACCESS_KEY"),
			PathStyle:       env("BUCKET_PATH_STYLE") == "1",
		},
		LogLevel:      parseLevel(os.Getenv("LOG_LEVEL")),
		Port:          envOr("PORT", "8080"),
		Deployment:    deployment,
		DataDir:       filepath.Join(os.TempDir(), "steki-db"),
		CreateEmptyDB: env("DB_CREATE_EMPTY") == "1",
		PostHogKey:    env("POSTHOG_API_KEY"),
		PostHogHost:   envOr("POSTHOG_HOST", "https://eu.i.posthog.com"),
	}
}

func checkSecret(name, v string) error {
	// Every link and token is signed from these, so a guessable one hands out
	// members' identities. 32 hex characters (openssl rand -hex 16) is the least
	// that holds up.
	if len(v) < MinSecretLen {
		return fmt.Errorf("%s must be at least %d characters: generate one with `openssl rand -hex 32`", name, MinSecretLen)
	}
	return nil
}

func checkPublicURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
		return fmt.Errorf("PUBLIC_URL must be an https origin (got %q)", v)
	}
	return nil
}

// LoadRegistration reads the registration service's configuration.
func LoadRegistration() (*Registration, error) {
	var l loader
	cfg := &Registration{
		Common:        l.common(),
		ResendAPIKey:  l.need("RESEND_API_KEY"),
		EmailFrom:     envOr("EMAIL_FROM", "Steki <steki@poiw.org>"),
		PublicURL:     strings.TrimRight(l.need("PUBLIC_URL"), "/"),
		ModerationURL: l.need("MODERATION_URL"),
		WebURL:        l.need("WEB_URL"),
	}
	if err := l.err(); err != nil {
		return nil, err
	}
	if err := checkSecret("INTERNAL_SECRET", cfg.InternalSecret); err != nil {
		return nil, err
	}
	return cfg, checkPublicURL(cfg.PublicURL)
}

// LoadModeration reads the moderation service's configuration.
func LoadModeration() (*Moderation, error) {
	var l loader
	cfg := &Moderation{
		Common:          l.common(),
		PublicURL:       strings.TrimRight(l.need("PUBLIC_URL"), "/"),
		RegistrationURL: l.need("REGISTRATION_URL"),
		WebURL:          l.need("WEB_URL"),
	}
	for _, entry := range strings.Split(os.Getenv("URL_BLACKLIST"), ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			cfg.URLBlacklist = append(cfg.URLBlacklist, strings.ToLower(entry))
		}
	}
	if err := l.err(); err != nil {
		return nil, err
	}
	if err := checkSecret("INTERNAL_SECRET", cfg.InternalSecret); err != nil {
		return nil, err
	}
	return cfg, checkPublicURL(cfg.PublicURL)
}

// LoadWeb reads the web service's configuration.
func LoadWeb() (*Web, error) {
	var l loader
	cfg := &Web{
		Common:               l.common(),
		JWTSecret:            l.need("JWT_SECRET"),
		PublicURL:            publicURL(),
		RailwayToken:         env("RAILWAY_TOKEN"),
		RailwayServiceID:     env("RAILWAY_SERVICE_ID"),
		RailwayEnvironmentID: env("RAILWAY_ENVIRONMENT_ID"),
		DiscordClientID:      env("DISCORD_CLIENT_ID"),
		DiscordClientSecret:  env("DISCORD_CLIENT_SECRET"),
		GoogleAIToken:        env("GOOGLE_AI_TOKEN"),
		GoogleAIModel:        env("GOOGLE_AI_MODEL"),
		BlastdoorURL:         env("BLASTDOOR_URL"),
		RegistrationURL:      l.need("REGISTRATION_URL"),
		ModerationURL:        l.need("MODERATION_URL"),
	}
	if m := env("RAILWAY_VOLUME_MOUNT_PATH"); m != "" {
		cfg.LegacyDatabasePath = filepath.Join(m, "steki.db")
	}
	if cfg.PublicURL == "" {
		l.missing = append(l.missing, "PUBLIC_URL (or a Railway public domain)")
	}
	if err := l.err(); err != nil {
		return nil, err
	}
	for name, v := range map[string]string{"JWT_SECRET": cfg.JWTSecret, "INTERNAL_SECRET": cfg.InternalSecret} {
		if err := checkSecret(name, v); err != nil {
			return nil, err
		}
	}
	return cfg, checkPublicURL(cfg.PublicURL)
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
