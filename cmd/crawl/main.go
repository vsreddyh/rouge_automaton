// Command crawl refreshes the unit and world shelves from the wiki, one
// page per shelf member. It is the polite half of the watcher: the RecentChanges
// feed says what changed, the crawl says what the store is missing.
//
// Gear is not crawled — the catalog owns it (see cmd/catalog).
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

	"rouge_automaton/internal/config"
	"rouge_automaton/internal/wikifeed"
	"rouge_automaton/internal/wikiwatch"
)

func main() {
	// run() returns errors instead of calling log.Fatalf so deferred
	// cleanup (mongo Disconnect) always executes; only main may exit.
	if err := run(); err != nil {
		log.Fatalf("crawl: %v", err)
	}
}

func run() error {
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	interval := flag.Duration("interval", 720*time.Hour, "crawl interval")
	delay := flag.Duration("delay", time.Second, "pause between page fetches")
	limit := flag.Int("limit", 0, "max pages per shelf (0 means no cap)")
	once := flag.Bool("once", false, "crawl once and exit")
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
	w := wikiwatch.New(db, wikifeed.NewWithUA(cfg.WikiUserAgent))
	w.CrawlDelay = *delay
	w.CrawlLimit = *limit

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		return report(w.Crawl(ctx))
	}
	log.Printf("crawl: crawling every %s", *interval)
	for {
		if err := report(w.Crawl(ctx)); err != nil {
			log.Printf("crawl: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

// report prints one line per crawled shelf.
func report(results []wikiwatch.CrawlResult, err error) error {
	for _, r := range results {
		fmt.Printf("crawl: %s attempts=%d updated=%d unchanged=%d missing=%d failed=%d\n",
			r.Shelf, r.Attempts, r.Updated, r.Unchanged, r.Missing, r.Failed)
	}
	return err
}
