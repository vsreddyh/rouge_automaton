// Package config loads the bot's runtime configuration from the environment.
package config

import (
	"net/url"
	"os"
	"strings"
)

// Config mirrors the Python bot's config.py so both implementations share
// the same .env contract.
type Config struct {
	DiscordBotToken      string
	AllowedUsers         map[string]bool
	AllowAllUsers        bool
	FreeResponseChannels map[string]bool
	MongoURI             string
	Model                string
	OpenCodeGoAPIKey     string
	OpenCodeGoBaseURL    string
	RequireMention       bool
	AutoThread           bool
	HD2APIBase           string
	XSuperClient         string
	XSuperContact        string
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func strSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

// Load reads the environment into a Config.
func Load() Config {
	return Config{
		DiscordBotToken:      os.Getenv("DISCORD_BOT_TOKEN"),
		AllowedUsers:         strSet(os.Getenv("DISCORD_ALLOWED_USERS")),
		AllowAllUsers:        strings.ToLower(os.Getenv("DISCORD_ALLOW_ALL_USERS")) == "true",
		FreeResponseChannels: strSet(getenv("DISCORD_FREE_RESPONSE_CHANNELS", "1547681840628764854")),
		MongoURI:             getenv("MONGO_URI", "mongodb://mongo:27017/rouge"),
		Model:                getenv("MODEL", "muse-spark-1.3-contributor"),
		OpenCodeGoAPIKey:     os.Getenv("OPENCODE_GO_API_KEY"),
		OpenCodeGoBaseURL:    getenv("OPENCODE_GO_BASE_URL", "https://opencode.ai/zen/go"),
		RequireMention:       true,
		AutoThread:           true,
		HD2APIBase:           getenv("HD2_API_BASE", "https://api.helldivers2.dev/api/v1"),
		XSuperClient:         getenv("X_SUPER_CLIENT", "rouge-automaton"),
		XSuperContact:        getenv("X_SUPER_CONTACT", "admin@example.com"),
	}
}

// Allowed reports whether a Discord user ID may use the bot.
func (c Config) Allowed(userID string) bool {
	return c.AllowAllUsers || c.AllowedUsers[userID]
}

// DBName returns the Mongo database name from the URI path, defaulting to rouge.
func (c Config) DBName() string {
	u, err := url.Parse(c.MongoURI)
	if err != nil || u.Path == "" || u.Path == "/" {
		return "rouge"
	}
	return strings.TrimPrefix(u.Path, "/")
}
