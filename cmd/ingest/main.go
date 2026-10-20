// Command ingest seeds Mongo from skills/rouge-automaton/data/*.json.
// Safe to re-run: seed fields apply on insert only ($setOnInsert), so
// wiki-enriched docs from the crawler are never clobbered.
//
// TODO: seed files cover units and structures only; gear (weapons,
// stratagems, armor, boosters) and world (biomes, planets, missions,
// difficulty, effects) collections still rely on the wiki crawl until
// curated seed files land (same data/*.json shape, $setOnInsert semantics).
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

	"rouge_automaton/internal/config"
)

type unitSeed struct {
	Name       string   `json:"name"`
	Aliases    []string `json:"aliases"`
	Role       string   `json:"role"`
	WeakPoints []string `json:"weak_points"`
	Counters   []string `json:"counters"`
}

type structureSeed struct {
	Name     string   `json:"name"`
	Counters []string `json:"counters"`
	Note     string   `json:"note"`
}

type dataFile struct {
	Faction    string          `json:"faction"`
	Units      []unitSeed      `json:"units"`
	Structures []structureSeed `json:"structures"`
}

func main() {
	// run() returns errors instead of calling log.Fatalf so deferred
	// cleanup (mongo Disconnect) always executes; only main may exit.
	if err := run(); err != nil {
		log.Fatalf("ingest: %v", err)
	}
}

// ciCollation backs exact-name identity end to end: the unique indexes and
// the upsert filters both compare case-insensitively, matching the
// server-side normalize/isExactMatch.
var ciCollation = &options.Collation{Locale: "en", Strength: 2}

// ensureIndexes creates the unique indexes the exact tools rely on:
// (faction, name) for units and structures, name for gear and world
// collections. Strength-2 collation makes uniqueness case-insensitive to
// match the server-side exact lookup (normalize/isExactMatch); without it
// "Hulk" and "hulk" could both insert and the winner would be sort order.
// Explicit ci names plus a best-effort drop of the pre-collation legacy
// indexes (auto names): re-running ingest where the legacy index exists
// must stay a no-op, never an IndexOptionsConflict.
func ensureIndexes(ctx context.Context, db *mongo.Database) error {
	pair := mongo.IndexModel{
		Keys: bson.D{{Key: "faction", Value: 1}, {Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("faction_name_ci").
			SetCollation(ciCollation),
	}
	single := mongo.IndexModel{
		Keys: bson.D{{Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("name_ci").
			SetCollation(ciCollation),
	}
	// dropLegacy removes the pre-collation index if present; absence (or
	// any other drop error) is not fatal — the ci index above is the
	// enforcement point either way.
	dropLegacy := func(coll, name string) {
		if _, err := db.Collection(coll).Indexes().DropOne(ctx, name); err != nil {
			log.Printf("ingest: drop legacy index %s.%s: %v", coll, name, err)
		}
	}
	for _, coll := range []string{"units", "structures"} {
		if _, err := db.Collection(coll).Indexes().CreateOne(ctx, pair); err != nil {
			return fmt.Errorf("index %s: %w", coll, err)
		}
		dropLegacy(coll, "faction_1_name_1")
	}
	for _, coll := range []string{"weapons", "stratagems", "armor", "boosters",
		"biomes", "planets", "missions", "difficulty", "effects"} {
		if _, err := db.Collection(coll).Indexes().CreateOne(ctx, single); err != nil {
			return fmt.Errorf("index %s: %w", coll, err)
		}
		dropLegacy(coll, "name_1")
	}
	return nil
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
	if err := ensureIndexes(ctx, db); err != nil {
		return err
	}
	// name -> aliases per collection for coverage checks. Project only what
	// covered() needs: pulling whole docs here would drag wiki_text/blob
	// payloads for a membership test.
	loadKnown := func(collName string) (map[string][]string, error) {
		cur, err := db.Collection(collName).Find(ctx, bson.M{},
			options.Find().SetProjection(bson.M{"name": 1, "aliases": 1}))
		if err != nil {
			return nil, err
		}
		var found []bson.M
		allErr := cur.All(ctx, &found)
		closeErr := cur.Close(ctx)
		if allErr != nil {
			return nil, allErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		known := map[string][]string{}
		for _, d := range found {
			if n, ok := d["name"].(string); ok {
				// Alias fields come in every writer shape; coerce the same
				// scalar set toString accepts so covered() never silently
				// misses a differently-shaped doc.
				var raw []any
				switch a := d["aliases"].(type) {
				case bson.A:
					raw = []any(a)
				case []any:
					raw = a
				case []string:
					raw = make([]any, len(a))
					for i, s := range a {
						raw[i] = s
					}
				case json.Number:
					raw = []any{string(a)}
				case string:
					// A lone string alias (same shape strList accepts).
					raw = []any{a}
				case bson.D:
					for _, e := range a {
						raw = append(raw, e.Value)
					}
				}
				var al []string
				for _, e := range raw {
					var s string
					switch t := e.(type) {
					case string:
						s = t
					case bool,
						int, int8, int16, int32, int64,
						uint, uint8, uint16, uint32, uint64,
						float32, float64,
						json.Number:
						s = fmt.Sprint(t)
					default:
						continue
					}
					if s = strings.TrimSpace(s); s != "" {
						al = append(al, strings.ToLower(s))
					}
				}
				known[strings.ToLower(strings.TrimSpace(n))] = al
			}
		}
		return known, nil
	}
	knownUnits, err := loadKnown("units")
	if err != nil {
		return err
	}
	knownStructures, err := loadKnown("structures")
	if err != nil {
		return err
	}
	// covered reports whether a combined seed name (e.g. "Charger / Behemoth")
	// is already represented by standalone docs in the same collection,
	// matched on exact /-tokens against known names and aliases.
	covered := func(known map[string][]string, seed string) bool {
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
	var unitOps []mongo.WriteModel
	var structOps []mongo.WriteModel
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
			if covered(knownUnits, u.Name) {
				continue
			}
			seed := bson.M{
				"faction": faction, "name": u.Name, "aliases": u.Aliases,
				"role": u.Role, "weak_points": u.WeakPoints, "counters": u.Counters,
				"source":        "data/" + filepath.Base(f),
				"source_url":    "",
				"rev_id":        0,
				"patch_version": "",
				// No text_blob: the fuzzy ranker that read it is gone, and
				// findAll excludes it — seeds carry only curated fields.
				// No fetched_at: absent until the watcher writes a real
				// timestamp. An explicit null would sit beside strings in
				// the same field across docs.
			}
			unitOps = append(unitOps, mongo.NewUpdateOneModel().
				SetFilter(bson.M{"faction": faction, "name": u.Name}).
				SetUpdate(bson.M{"$setOnInsert": seed}).
				SetCollation(ciCollation).
				SetUpsert(true))
		}
		for _, s := range d.Structures {
			if s.Name == "" || covered(knownStructures, s.Name) {
				continue
			}
			seed := bson.M{
				"faction": faction, "name": s.Name, "counters": s.Counters,
				"note": s.Note, "role": s.Note,
				"source":        "data/" + filepath.Base(f),
				"source_url":    "",
				"rev_id":        0,
				"patch_version": "",
				// No text_blob: see the units seed above.
			}
			structOps = append(structOps, mongo.NewUpdateOneModel().
				SetFilter(bson.M{"faction": faction, "name": s.Name}).
				SetUpdate(bson.M{"$setOnInsert": seed}).
				SetCollation(ciCollation).
				SetUpsert(true))
		}
	}
	if len(unitOps) == 0 && len(structOps) == 0 {
		fmt.Println("ingest: nothing to do")
		return nil
	}
	// Fresh timeout for the write phase: the 60s ctx above already covered
	// indexes plus coverage loads, and a growing seed set must not deadline
	// the writes.
	wctx, wcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer wcancel()
	var upserted, matched int64
	if len(unitOps) > 0 {
		res, err := db.Collection("units").BulkWrite(wctx, unitOps)
		if err != nil {
			return err
		}
		upserted += res.UpsertedCount
		matched += res.MatchedCount
	}
	if len(structOps) > 0 {
		res, err := db.Collection("structures").BulkWrite(wctx, structOps)
		if err != nil {
			return err
		}
		upserted += res.UpsertedCount
		matched += res.MatchedCount
	}
	fmt.Printf("ingest: upserted=%d matched=%d\n", upserted, matched)
	return nil
}
