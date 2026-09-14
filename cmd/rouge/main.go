// Command rouge is the Helldivers 2 Discord roleplay bot.
// Wiring lands in later PRs; this stub validates the environment contract.
package main

import (
	"fmt"
	"os"

	"github.com/vsreddyh/rouge_automaton/internal/config"
)

func main() {
	cfg := config.Load()
	if cfg.DiscordBotToken == "" {
		fmt.Fprintln(os.Stderr, "DISCORD_BOT_TOKEN is required")
		os.Exit(1)
	}
	if cfg.OpenCodeGoAPIKey == "" {
		fmt.Fprintln(os.Stderr, "OPENCODE_GO_API_KEY is required")
		os.Exit(1)
	}
	fmt.Printf("rouge-automaton go skeleton: model=%s mongo=%s\n", cfg.Model, cfg.MongoURI)
}
