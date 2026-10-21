package config

import (
	"os"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	for _, k := range []string{"DISCORD_FREE_RESPONSE_CHANNELS", "DISCORD_ALLOWED_USERS",
		"DISCORD_ALLOW_ALL_USERS", "MONGO_URI", "MODEL", "OPENCODE_GO_BASE_URL",
		"OPENCODE_GO_API_KEY", "DISCORD_BOT_TOKEN", "HD2_API_BASE",
		"X_SUPER_CLIENT", "X_SUPER_CONTACT", "WIKI_USER_AGENT"} {
		t.Setenv(k, "")
	}
	// Empty is set-but-empty: must be honored, not replaced by defaults.
	c := Load()
	if len(c.FreeResponseChannels) != 0 {
		t.Fatalf("empty channels must stay empty, got %v", c.FreeResponseChannels)
	}
	for _, k := range []string{"DISCORD_FREE_RESPONSE_CHANNELS", "MONGO_URI", "MODEL",
		"OPENCODE_GO_BASE_URL", "HD2_API_BASE", "X_SUPER_CLIENT", "X_SUPER_CONTACT",
		"WIKI_USER_AGENT"} {
		os.Unsetenv(k)
	}
	c = Load()
	if c.Model != "muse-spark-1.3-contributor" {
		t.Fatalf("default model = %q", c.Model)
	}
	if c.OpenCodeGoBaseURL != "https://opencode.ai/zen/go" {
		t.Fatalf("default base = %q", c.OpenCodeGoBaseURL)
	}
	if !c.RequireMention || !c.AutoThread {
		t.Fatal("mention/thread defaults must be true")
	}
	if !strings.Contains(c.WikiUserAgent, "vsreddyh") || !strings.Contains(c.WikiUserAgent, "@") {
		t.Fatalf("default wiki UA must name the account and a contact: %q", c.WikiUserAgent)
	}
	if len(c.FreeResponseChannels) != 1 {
		t.Fatalf("default free channels = %v", c.FreeResponseChannels)
	}
}

func TestAllowlist(t *testing.T) {
	t.Setenv("DISCORD_ALLOWED_USERS", "1, 2")
	c := Load()
	if !c.Allowed("1") || c.Allowed("3") {
		t.Fatal("allowlist mismatch")
	}
	t.Setenv("DISCORD_ALLOW_ALL_USERS", "true")
	if !Load().Allowed("anyone") {
		t.Fatal("allow-all must permit everyone")
	}
}
