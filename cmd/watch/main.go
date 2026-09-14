// Command watch polls the Helldivers wiki RecentChanges Atom feed and
// refreshes changed docs in Mongo. Replaces the Python rss_watch.py.
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

	"github.com/vsreddyh/rouge_automaton/internal/config"
	"github.com/vsreddyh/rouge_automaton/internal/wikifeed"
	"github.com/vsreddyh/rouge_automaton/internal/wikiwatch"
)

func main() {
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	interval := flag.Duration("interval", 5*time.Minute, "poll interval")
	once := flag.Bool("once", false, "poll once and exit")
	days := flag.Int("days", 1, "feed lookback window in days")
	limit := flag.Int("limit", 50, "max feed entries per poll")
	flag.Parse()
	if strings.TrimSpace(*mongoURI) == "" {
		*mongoURI = "mongodb://localhost:27017/rouge"
	}
	client, err := mongo.Connect(options.Client().ApplyURI(*mongoURI))
	if err != nil {
		log.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(context.Background())
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		log.Fatalf("mongo ping: %v", err)
	}
	db := client.Database(config.Config{MongoURI: *mongoURI}.DBName())
	w := wikiwatch.New(db, wikifeed.New())
	w.Days, w.Limit = *days, *limit

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	seen, err := w.Backfill(ctx)
	if err != nil {
		log.Printf("watch: backfill: %v", err)
	}
	log.Printf("watch: backfill tracked %d doc(s)", seen)

	if *once {
		n, err := w.RunOnce(ctx)
		if err != nil {
			log.Fatalf("watch: %v", err)
		}
		fmt.Printf("watch: %d change(s)\n", n)
		return
	}
	log.Printf("watch: polling every %s", *interval)
	w.Run(ctx, *interval)
}
