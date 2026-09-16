// Command rouge is the Helldivers 2 Discord roleplay bot.
package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/vsreddyh/rouge_automaton/internal/config"
	godiscord "github.com/vsreddyh/rouge_automaton/internal/discord"
)

func main() {
	// run() returns errors instead of calling log.Fatalf so deferred
	// cleanup (mongo Disconnect) always executes; only main may exit.
	if err := run(); err != nil {
		log.Fatalf("rouge: %v", err)
	}
}

func run() error {
	cfg := config.Load()
	if strings.TrimSpace(cfg.DiscordBotToken) == "" || strings.TrimSpace(cfg.OpenCodeGoAPIKey) == "" {
		return fmt.Errorf("DISCORD_BOT_TOKEN and OPENCODE_GO_API_KEY are required")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		return fmt.Errorf("mongo: %w", err)
	}
	defer client.Disconnect(context.Background())
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	bot, err := godiscord.New(cfg, client.Database(cfg.DBName()))
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := bot.Start(ctx); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	return nil
}
