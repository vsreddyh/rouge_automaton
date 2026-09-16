// Command ingest seeds Mongo from skills/rouge-automaton/data/*.json.
// Safe to re-run: seed fields apply on insert only ($setOnInsert), so
// wiki-enriched docs from the crawler are never clobbered.
//
// TODO: seed files carry units only; the structures collection has no seed
// source (same gap as the Python scripts).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/vsreddyh/rouge_automaton/internal/config"
)

type dataFile struct {
	Faction string `json:"faction"`
	Units   []struct {
		Name       string   `json:"name"`
		Aliases    []string `json:"aliases"`
		Role       string   `json:"role"`
		WeakPoints []string `json:"weak_points"`
		Counters   []string `json:"counters"`
	} `json:"units"`
}

func main() {
	// run() returns errors instead of calling log.Fatalf so deferred
	// cleanup (mongo Disconnect) always executes; only main may exit.
	if err := run(); err != nil {
		log.Fatalf("ingest: %v", err)
	}
}

func run() error {
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	dataDir := flag.String("data", "skills/rouge-automaton/data", "seed JSON dir")
	flag.Parse()
	if *mongoURI == "" {
		*mongoURI = "mongodb://localhost:27017/rouge"
	}
	files, err := filepath.Glob(filepath.Join(*dataDir, "*.json"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("no seed files in %s", *dataDir)
	}
	client, err := mongo.Connect(options.Client().ApplyURI(*mongoURI))
	if err != nil {
		return err
	}
	defer client.Disconnect(context.Background())
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	db := client.Database(config.Config{MongoURI: *mongoURI}.DBName())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	coll := db.Collection("units")
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "faction", Value: 1}, {Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	// name -> aliases for coverage checks. Project only what covered()
	// needs: pulling whole docs here would drag wiki_text/blob payloads
	// for a membership test.
	cur, err := coll.Find(ctx, bson.M{},
		options.Find().SetProjection(bson.M{"name": 1, "aliases": 1}))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	var found []bson.M
	if err := cur.All(ctx, &found); err != nil {
		return err
	}
	known := map[string][]string{}
	for _, d := range found {
		if n, ok := d["name"].(string); ok {
			var al []string
			if a, ok := d["aliases"].(bson.A); ok {
				for _, e := range a {
					if s, ok := e.(string); ok {
						al = append(al, strings.ToLower(s))
					}
				}
			}
			known[strings.ToLower(n)] = al
		}
	}
	// covered reports whether a combined seed name (e.g. "Charger / Behemoth")
	// is already represented by standalone docs, matched on exact /-tokens
	// against known names and aliases.
	covered := func(seed string) bool {
		toks := map[string]bool{}
		for _, t := range strings.Split(strings.ToLower(seed), "/") {
			if t = strings.TrimSpace(t); t != "" {
				toks[t] = true
			}
		}
		for n, al := range known {
			if n == strings.ToLower(seed) {
				continue
			}
			if toks[n] {
				return true
			}
			for _, a := range al {
				if toks[a] {
					return true
				}
			}
		}
		return false
	}
	var ops []mongo.WriteModel
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var d dataFile
		if err := json.Unmarshal(raw, &d); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		faction := strings.ToLower(d.Faction)
		if faction == "" {
			faction = strings.ToLower(strings.TrimSuffix(filepath.Base(f), ".json"))
		}
		for _, u := range d.Units {
			if covered(u.Name) {
				continue
			}
			parts := append([]string{u.Name, faction}, u.Aliases...)
			parts = append(parts, u.Role)
			parts = append(parts, u.WeakPoints...)
			parts = append(parts, u.Counters...)
			seed := bson.M{
				"faction": faction, "name": u.Name, "aliases": u.Aliases,
				"role": u.Role, "weak_points": u.WeakPoints, "counters": u.Counters,
				"text_blob":     strings.Join(parts, " | "),
				"source":        "data/" + filepath.Base(f),
				"source_url":    "",
				"rev_id":        0,
				"patch_version": "",
				// No fetched_at: absent until the watcher writes a real
				// timestamp. An explicit null would sit beside strings in
				// the same field across docs.
			}
			ops = append(ops, mongo.NewUpdateOneModel().
				SetFilter(bson.M{"faction": faction, "name": u.Name}).
				SetUpdate(bson.M{"$setOnInsert": seed}).
				SetUpsert(true))
		}
	}
	if len(ops) == 0 {
		fmt.Println("ingest: nothing to do")
		return nil
	}
	res, err := coll.BulkWrite(ctx, ops)
	if err != nil {
		return err
	}
	fmt.Printf("ingest: upserted=%d matched=%d\n", res.UpsertedCount, res.MatchedCount)
	return nil
}
