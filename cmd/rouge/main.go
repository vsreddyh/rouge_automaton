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

	"rouge_automaton/internal/config"
	godiscord "rouge_automaton/internal/discord"
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
	// Fail fast on a non-loopback test-door bind: refuse to boot before
	// touching the network. An unset token means the door stays shut.
	// The token is trimmed once here so the enable check and the bearer
	// compare below can never disagree (" secret " must not enable an
	// unauthenticatable door).
	backdoorToken := strings.TrimSpace(cfg.LocalBackdoorToken)
	// Trim the bind once: Validate trims internally, but Listen uses the
	// raw value, so an untrimmed addr could pass validation then fail to
	// bind. One trimmed copy feeds both.
	backdoorAddr := strings.TrimSpace(cfg.LocalBackdoorAddr)
	if backdoorToken != "" {
		if err := godiscord.ValidateBackdoorAddr(backdoorAddr); err != nil {
			return err
		}
	} else {
		log.Printf("rouge: local backdoor disabled (LOCAL_BACKDOOR_TOKEN unset)")
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
	// The local test door never changes gateway behavior: with the token
	// unset no listener starts (logged above); otherwise serve loopback in
	// the background while the gateway blocks below.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if backdoorToken != "" {
		go func() {
			if err := bot.StartBackdoor(ctx, backdoorAddr, backdoorToken); err != nil {
				log.Printf("rouge: LOCAL BACKDOOR FAILED, gateway continuing: %v", err)
			}
		}()
	}
	if err := bot.Start(ctx); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	return nil
}
