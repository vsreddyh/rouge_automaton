// Command catalog syncs the gear shelves (weapons, stratagems, armor,
// boosters) from the public gear catalog: helldivers-api.dionatha.com.br,
// a daily snapshot of helldivers.wiki.gg republished as static JSON.
//
// The catalog knows every gear item's wiki page, so the gear shelves are
// refreshed without asking the wiki anything — and an unchanged snapshot
// costs one 304 per collection. The payload is cached in Mongo, so a
// catalog outage costs freshness, not answers.
package main

import (
	"context"
	"flag"
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

	"rouge_automaton/internal/catalog"
	"rouge_automaton/internal/config"
)

func main() {
	// run() returns errors instead of calling log.Fatalf so deferred
	// cleanup (mongo Disconnect) always executes; only main may exit.
	if err := run(); err != nil {
		log.Fatalf("catalog: %v", err)
	}
}

func run() error {
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	interval := flag.Duration("interval", 24*time.Hour, "sync interval")
	once := flag.Bool("once", false, "sync once and exit")
	flag.Parse()
	if strings.TrimSpace(*mongoURI) == "" {
		*mongoURI = "mongodb://localhost:27017/rouge"
	}
	client, err := mongo.Connect(options.Client().ApplyURI(*mongoURI))
	if err != nil {
		return fmt.Errorf("mongo: %w", err)
	}
	defer client.Disconnect(context.Background())
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	cfg := config.Load()
	db := client.Database(config.DBNameFromURI(*mongoURI))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		return report(catalog.Sync(ctx, catalog.Options{
			DB:     db,
			Client: catalog.NewWithUA(cfg.CatalogUserAgent),
		}))
	}
	log.Printf("catalog: syncing every %s", *interval)
	for {
		if err := report(catalog.Sync(ctx, catalog.Options{
			DB:     db,
			Client: catalog.NewWithUA(cfg.CatalogUserAgent),
		})); err != nil {
			log.Printf("catalog: sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

// report prints one line per synced shelf, the way cmd/ingest does.
func report(results []catalog.Result, err error) error {
	for _, r := range results {
		if r.Shelf == "" {
			for _, s := range r.Sources {
				if s.Err != "" {
					log.Printf("catalog: %s: %s", s.Collection, s.Err)
					continue
				}
				log.Printf("catalog: %s: %d item(s)%s", s.Collection, s.Count, notModified(s))
			}
			continue
		}
		fmt.Printf("catalog: %s items=%d updated=%d unchanged=%d new=%d removed=%d mismatches=%d failed=%d\n",
			r.Shelf, r.Items, r.Updated, r.Unchanged, r.New, r.Removed, r.Mismatches, r.Failed)
		if r.Err != "" {
			log.Printf("catalog: %s: %s", r.Shelf, r.Err)
		}
		for _, s := range r.Sources {
			if s.Err != "" {
				log.Printf("catalog: %s: %s", s.Collection, s.Err)
			}
		}
	}
	return err
}

func notModified(s catalog.Fetch) string {
	switch {
	case s.NotModified:
		return " (unchanged snapshot)"
	case s.Cached:
		return " (cached: the catalog was unreachable)"
	}
	return ""
}
