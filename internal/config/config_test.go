package config

import (
	"testing"
)

func TestDefaults(t *testing.T) {
	t.Setenv("DISCORD_FREE_RESPONSE_CHANNELS", "")
	c := Load()
	if c.Model != "muse-spark-1.3-contributor" {
		t.Fatalf("default model = %q", c.Model)
	}
	if c.OpenCodeGoBaseURL != "https://opencode.ai/zen/go" {
		t.Fatalf("default base = %q", c.OpenCodeGoBaseURL)
	}
	if !c.RequireMention || !c.AutoThread {
		t.Fatal("mention/thread defaults must be true")
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
