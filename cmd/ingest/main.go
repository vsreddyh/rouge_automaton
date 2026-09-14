// Command ingest seeds Mongo from skills/rouge-automaton/data/*.json.
// Safe to re-run: seed fields apply on insert only ($setOnInsert), so
// wiki-enriched docs from the crawler are never clobbered.
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
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	dataDir := flag.String("data", "skills/rouge-automaton/data", "seed JSON dir")
	flag.Parse()
	if *mongoURI == "" {
		*mongoURI = "mongodb://localhost:27017/rouge"
	}
	files, err := filepath.Glob(filepath.Join(*dataDir, "*.json"))
	if err != nil || len(files) == 0 {
		log.Fatalf("no seed files in %s", *dataDir)
	}
	client, err := mongo.Connect(options.Client().ApplyURI(*mongoURI))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	dbName := "rouge"
	if i := strings.LastIndex(*mongoURI, "/"); i >= 0 {
		if n := strings.SplitN((*mongoURI)[i+1:], "?", 2)[0]; n != "" {
			dbName = n
		}
	}
	db := client.Database(dbName)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	existing := map[string]bool{}
	cur, err := db.Collection("units").Find(ctx, bson.M{})
	if err != nil {
		log.Fatal(err)
	}
	var found []bson.M
	if err := cur.All(ctx, &found); err != nil {
		log.Fatal(err)
	}
	cur.Close(ctx)
	for _, d := range found {
		if n, ok := d["name"].(string); ok {
			existing[strings.ToLower(n)] = true
		}
	}
	// covered reports whether a seed name is already represented by a
	// standalone wiki doc (e.g. "Charger / Behemoth" vs "Charger"), in which
	// case the combined seed must not be resurrected.
	covered := func(seed string) bool {
		s := strings.ToLower(seed)
		for n := range existing {
			if n != s && (strings.Contains(s, n) || strings.Contains(n, s)) {
				return true
			}
		}
		return false
	}
	var ops []mongo.WriteModel
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			log.Fatal(err)
		}
		var d dataFile
		if err := json.Unmarshal(raw, &d); err != nil {
			log.Fatalf("%s: %v", f, err)
		}
		faction := strings.ToLower(d.Faction)
		if faction == "" {
			faction = strings.TrimSuffix(filepath.Base(f), ".json")
		}
		for _, u := range d.Units {
			if covered(u.Name) {
				continue
			}
			seed := bson.M{
				"faction": faction, "name": u.Name, "aliases": u.Aliases,
				"role": u.Role, "weak_points": u.WeakPoints, "counters": u.Counters,
				"text_blob": strings.Join(append([]string{u.Name, faction},
					append(append(append([]string{}, u.Aliases...), u.Role),
						append(u.WeakPoints, u.Counters...)...)...), " | "),
				"source": "data/" + filepath.Base(f),
			}
			ops = append(ops, mongo.NewUpdateOneModel().
				SetFilter(bson.M{"faction": faction, "name": u.Name}).
				SetUpdate(bson.M{"$setOnInsert": seed}).
				SetUpsert(true))
		}
	}
	res, err := db.Collection("units").BulkWrite(ctx, ops)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("ingest: upserted=%d matched=%d\n", res.UpsertedCount, res.MatchedCount)
}
