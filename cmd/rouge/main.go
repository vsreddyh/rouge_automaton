// Command rouge is the Helldivers 2 Discord roleplay bot.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
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
	cfg := config.Load()
	if strings.TrimSpace(cfg.DiscordBotToken) == "" || strings.TrimSpace(cfg.OpenCodeGoAPIKey) == "" {
		fmt.Fprintln(os.Stderr, "DISCORD_BOT_TOKEN and OPENCODE_GO_API_KEY are required")
		os.Exit(1)
	}
	client, err := mongo.Connect(options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		log.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(context.Background())
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		log.Fatalf("mongo ping: %v", err)
	}
	bot, err := godiscord.New(cfg, client.Database(cfg.DBName()))
	if err != nil {
		log.Fatalf("discord: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := bot.Start(ctx); err != nil {
		log.Fatalf("gateway: %v", err)
	}
}
