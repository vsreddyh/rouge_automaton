// Package config loads the bot's runtime configuration from the environment.
package config

import (
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is the bot's full runtime configuration. Every value comes from
// the environment so the Python and Go implementations share one .env
// contract (.env.example); see Load for the variable names and defaults.
type Config struct {
	// DiscordBotToken authenticates the gateway session ("Bot "+token).
	DiscordBotToken string
	// AllowedUsers is the DISCORD_ALLOWED_USERS allowlist, keyed by user ID.
	AllowedUsers map[string]bool
	// AllowAllUsers bypasses the allowlist when DISCORD_ALLOW_ALL_USERS=true.
	AllowAllUsers bool
	// FreeResponseChannels are channel IDs where the bot answers without a
	// mention. Each entry multiplies model calls, so keep this list short.
	FreeResponseChannels map[string]bool
	// MongoURI points at the RAG store; the database name is taken from its path.
	MongoURI string
	// Model is the Zen Go model id; never silently downgraded on failure.
	Model string
	// OpenCodeGoAPIKey authorizes relay calls (Bearer).
	OpenCodeGoAPIKey string
	// OpenCodeGoBaseURL is the relay root; "/v1/responses" is appended per call.
	OpenCodeGoBaseURL string
	// RequireMention gates replies on @-mention outside free channels.
	RequireMention bool
	// AutoThread opens a thread per answered question for history scoping.
	AutoThread bool
	// HD2APIBase is the community live-war API root.
	HD2APIBase string
	// RateLimitPerMin refills caller quota per minute; RateBurst caps it.
	RateLimitPerMin int
	RateBurst       int
	// XSuperClient and XSuperContact are the headers the war API requires.
	XSuperClient  string
	XSuperContact string
	// WikiUserAgent identifies the crawler, the watcher and the fallback
	// client to wiki.gg. wiki.gg rate-limits clients it cannot identify, so
	// the default names the operator's wiki account and a reachable contact
	// (see WIKI_USER_AGENT in .env.example).
	WikiUserAgent string
	// CatalogUserAgent identifies the bot to the gear catalog host. The
	// catalog is a different host with its own rules, so it gets its own
	// knob; an empty value keeps the catalog package's default.
	CatalogUserAgent string
	// LocalBackdoorToken enables the loopback-only test door (POST
	// /converse). Empty disables it. Kept in untracked .env, never
	// logged or echoed.
	LocalBackdoorToken string
	// LocalBackdoorAddr is the loopback bind for the test door,
	// default 127.0.0.1:8099. Any non-loopback value refuses to boot.
	LocalBackdoorAddr string
}

// getenv returns the variable's value, defaulting only when it is unset.
// LookupEnv (not Getenv) is deliberate: an explicitly empty value is a real
// setting (e.g. DISCORD_FREE_RESPONSE_CHANNELS="" disables free replies),
// while an absent one falls back to the default.
func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// strSet parses a comma-separated env list into a membership set, dropping
// blanks and surrounding whitespace so "1, 2" and "1,2" behave the same.
// Used for DISCORD_ALLOWED_USERS (several IDs) and
// DISCORD_FREE_RESPONSE_CHANNELS (usually one ID, but the shape is a list).
func strSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

// getenvInt reads an int env var, defaulting on unset or unparsable input.
// A fallback on explicitly-set garbage logs once so misconfig is visible.
func getenvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
		log.Printf("config: %s=%q invalid, using default %d", key, v, def)
	}
	return def
}

// Load reads the process environment into a Config. Secrets keep whatever
// the environment holds (empty included); only the documented knobs above
// carry defaults. Mention/thread behavior is intentionally hardcoded, not
// env-toggled, to prevent accidental always-on billing.
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
		HD2APIBase:           getenv("HD2_API_BASE", "https://helldiverstrainingmanual.com/api/v1"),
		RateLimitPerMin:      getenvInt("RATE_LIMIT_PER_MIN", 10),
		RateBurst:            getenvInt("RATE_BURST", 3),
		XSuperClient:         getenv("X_SUPER_CLIENT", "rouge-automaton"),
		XSuperContact:        getenv("X_SUPER_CONTACT", "admin@example.com"),
		WikiUserAgent: getenv("WIKI_USER_AGENT",
			"rouge-automaton-watcher (wiki account: vsreddyh; contact: shouryanreddyh@gmail.com; use=reference)"),
		// Empty means "use the catalog package's own default": the
		// catalog host is not the wiki, and it has its own rules.
		CatalogUserAgent: getenv("CATALOG_USER_AGENT", ""),
		// Empty disables the door; a non-empty bearer enables a
		// loopback-only listener (see internal/discord/backdoor.go).
		LocalBackdoorToken: os.Getenv("LOCAL_BACKDOOR_TOKEN"),
		LocalBackdoorAddr:  getenv("LOCAL_BACKDOOR_ADDR", "127.0.0.1:8099"),
	}
}

// Allowed reports whether a Discord user ID may use the bot: allow-all mode
// admits everyone, otherwise the ID must be in the allowlist.
func (c Config) Allowed(userID string) bool {
	return c.AllowAllUsers || c.AllowedUsers[userID]
}

// DBName returns the Mongo database name from the URI path, defaulting to rouge.
func (c Config) DBName() string {
	return DBNameFromURI(c.MongoURI)
}

// DBNameFromURI returns the Mongo database name from a URI path, defaulting
// to rouge. The commands read the URI from a flag, which the config does
// not know about, so the name is derived from the URI they were given.
func DBNameFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Path == "" || u.Path == "/" {
		return "rouge"
	}
	return strings.Trim(strings.TrimPrefix(u.Path, "/"), "/")
}
