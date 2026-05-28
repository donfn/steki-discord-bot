package config

import (
	"os"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Env      string
	Token    string
	DevToken string

	FooterText string
	FooterIcon string
	Color      string
	Version    string

	MongoURL        string
	MailgunAPIToken string
	Salt            string

	URLBlacklist []string

	// Hardcoded Discord IDs (channel/category/role)
	LogChannelID       string
	WelcomeChannelID   string
	RegisterCategoryID string
}

func Load() *Config {
	_ = godotenv.Load()

	var blacklist []string
	if bl := os.Getenv("URL_BLACKLIST"); bl != "" {
		blacklist = strings.Split(bl, ",")
	}

	return &Config{
		Env:                getEnv("NODE_ENV", "production"),
		Token:              os.Getenv("TOKEN"),
		DevToken:           os.Getenv("DEV_TOKEN"),
		FooterText:         os.Getenv("FOOTER_TEXT"),
		FooterIcon:         os.Getenv("FOOTER_ICON"),
		Color:              os.Getenv("COLOR"),
		Version:            os.Getenv("VERSION"),
		MongoURL:           os.Getenv("MONGO_URL"),
		MailgunAPIToken:    os.Getenv("MAILGUN_API_TOKEN"),
		Salt:               os.Getenv("SALT"),
		URLBlacklist: blacklist,
		LogChannelID:       "939180081857839174",
		WelcomeChannelID:   "886981920108478526",
		RegisterCategoryID: "890021609631531009",
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
