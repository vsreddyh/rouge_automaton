// Command ingest seeds Mongo from skills/rouge-automaton/data/*.json.
// Safe to re-run: seed fields apply on insert only ($setOnInsert), so
// wiki-enriched docs from the crawler are never clobbered.
//
// Seed files cover units and structures (per faction) plus the gear shelves
// (weapons, stratagems, armor, boosters) and the world shelves (biomes,
// planets, missions, difficulty, effects). Gear and world docs share no
// schema, so they seed as free-form bags: name + aliases + fields.
package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"rouge_automaton/internal/mcpdb"
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

// docSeed is one curated gear or world doc: a canonical name, optional
// aliases, and a free-form bag of fields. The unit and structure seeds
// have fixed schemas; gear and world docs do not (a weapon carries an
// infobox, a biome carries hazards), so only name and aliases are lifted
// out and the rest of the bag becomes the Mongo doc body verbatim.
type docSeed struct {
	Name    string
	Aliases []string
	Fields  bson.M
}

// UnmarshalJSON lifts name and aliases out of a seed doc and keeps the
// remaining keys as the doc body. Aliases coerce through the same
// mcpdb.Values gate the store matches with, so a seed alias can never
// dodge the query-time lookup.
func (d *docSeed) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	d.Name = strings.TrimSpace(mcpdb.ToString(raw["name"]))
	d.Aliases = nil
	for _, a := range mcpdb.Values(raw["aliases"]) {
		if a = strings.TrimSpace(a); a != "" {
			d.Aliases = append(d.Aliases, a)
		}
	}
	delete(raw, "name")
	delete(raw, "aliases")
	d.Fields = bsonify(raw)
	return nil
}

// bsonifyValue converts a decoded JSON value into a BSON-safe value:
// nested objects become bson.M and arrays become bson.A, recursively, so a
// seed's nested params map lands as a document instead of an opaque blob.
func bsonifyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return bsonify(t)
	case []any:
		arr := make(bson.A, 0, len(t))
		for _, item := range t {
			arr = append(arr, bsonifyValue(item))
		}
		return arr
	default:
		return v
	}
}

// bsonify converts a decoded JSON bag into BSON-safe values.
func bsonify(v map[string]any) bson.M {
	out := bson.M{}
	for k, e := range v {
		out[k] = bsonifyValue(e)
	}
	return out
}

// reservedDocFields never come from a seed bag: Mongo owns _id, the wiki
// watcher owns fetched_at, and text_blob and embedding are excluded from
// every read path (the ranker that read them is gone).
var reservedDocFields = map[string]bool{
	"_id": true, "fetched_at": true, "text_blob": true, "embedding": true,
}

// seedDoc builds the Mongo body for one free-form seed: the field bag
// minus the reserved fields, then the identity and provenance the ingest
// owns. A seed must never set what Mongo or the wiki watcher owns.
func seedDoc(ds docSeed, source string) bson.M {
	seed := bson.M{}
	for k, v := range ds.Fields {
		if reservedDocFields[k] {
			continue
		}
		seed[k] = v
	}
	seed["name"] = ds.Name
	seed["aliases"] = ds.Aliases
	seed["source"] = source
	seed["source_url"] = ""
	seed["rev_id"] = 0
	seed["patch_version"] = ""
	return seed
}

// docCollections are the gear and world shelves seeded as free-form docs.
// The names are Mongo collection names, so the seed file keys match the
// collection each doc lands in.
var docCollections = []string{
	"weapons", "stratagems", "armor", "boosters",
	"biomes", "planets", "missions", "difficulty", "effects",
}

type dataFile struct {
	Faction    string          `json:"faction"`
	Units      []unitSeed      `json:"units"`
	Structures []structureSeed `json:"structures"`
	Weapons    []docSeed       `json:"weapons"`
	Stratagems []docSeed       `json:"stratagems"`
	Armor      []docSeed       `json:"armor"`
	Boosters   []docSeed       `json:"boosters"`
	Biomes     []docSeed       `json:"biomes"`
	Planets    []docSeed       `json:"planets"`
	Missions   []docSeed       `json:"missions"`
	Difficulty []docSeed       `json:"difficulty"`
	Effects    []docSeed       `json:"effects"`
}

// shelves returns the free-form doc shelves keyed by collection name.
func (d *dataFile) shelves() map[string][]docSeed {
	return map[string][]docSeed{
		"weapons": d.Weapons, "stratagems": d.Stratagems, "armor": d.Armor,
		"boosters": d.Boosters, "biomes": d.Biomes, "planets": d.Planets,
		"missions": d.Missions, "difficulty": d.Difficulty, "effects": d.Effects,
	}
}

func main() {
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	dataDir := flag.String("data", "skills/rouge-automaton/data", "seed JSON dir")
	flag.Parse()
	// run() takes plain values (not flags) so a second in-process call can
	// never panic on flag redefinition; only main touches flag state.
	// run() returns errors instead of calling log.Fatalf so deferred
	// cleanup (mongo Disconnect) always executes; only main may exit.
	if err := run(*mongoURI, *dataDir); err != nil {
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
	// dropLegacy removes the pre-collation index if present. Absence
	// (IndexNotFound, code 27) is the common clean-run case and stays
	// silent; any other drop error is logged but not fatal — the ci index
	// above is the enforcement point either way.
	dropLegacy := func(coll, name string) {
		if err := db.Collection(coll).Indexes().DropOne(ctx, name); err != nil {
			var cmdErr mongo.CommandError
			if errors.As(err, &cmdErr) && cmdErr.Code == 27 {
				return
			}
			log.Printf("ingest: drop legacy index %s.%s: %v", coll, name, err)
		}
	}
	for _, coll := range []string{"units", "structures"} {
		if _, err := db.Collection(coll).Indexes().CreateOne(ctx, pair); err != nil {
			return fmt.Errorf("index %s: %w", coll, err)
		}
		dropLegacy(coll, "faction_1_name_1")
	}
	for _, coll := range docCollections {
		if _, err := db.Collection(coll).Indexes().CreateOne(ctx, single); err != nil {
			return fmt.Errorf("index %s: %w", coll, err)
		}
		dropLegacy(coll, "name_1")
	}
	return nil
}

// covered reports whether a seed name is already represented in a
// collection: the same name is present, or the seed is a combined
// "A / B" name whose parts exist as standalone docs. Matching is on
// exact name or alias tokens, case-insensitively.
func covered(known map[string][]string, seed string) bool {
	toks := map[string]bool{}
	// Split on the " / " separator only: gear and weapon names carry
	// slashes inside the designation ("AR/GL-21 One-Two") and must stay
	// one token.
	for _, t := range strings.Split(strings.ToLower(seed), " / ") {
		if t = strings.TrimSpace(t); t != "" {
			toks[t] = true
		}
	}
	want := strings.ToLower(strings.TrimSpace(seed))
	for n, al := range known {
		if n == want {
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

func run(mongoURI, dataDir string) error {
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017/rouge"
	}
	files, err := filepath.Glob(filepath.Join(dataDir, "*.json"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("no seed files in %s", dataDir)
	}
	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		return err
	}
	defer client.Disconnect(context.Background())
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	db := client.Database(config.Config{MongoURI: mongoURI}.DBName())
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
		closeErr := cur.Close(context.WithoutCancel(ctx))
		if allErr != nil {
			return nil, allErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		known := map[string][]string{}
		for _, d := range found {
			// Names and aliases coerce through the same ToString gate the
			// store renders and matches with, so a non-string field can
			// never silently dodge coverage. Values is the single
			// shape-switch behind both this loader and query-time
			// strList, so the two sides cannot disagree.
			name := mcpdb.ToString(d["name"])
			if name == "" {
				continue
			}
			var al []string
			for _, s := range mcpdb.Values(d["aliases"]) {
				if s = strings.TrimSpace(s); s != "" {
					al = append(al, strings.ToLower(s))
				}
			}
			known[strings.ToLower(strings.TrimSpace(name))] = al
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
	// Gear and world shelves need the same per-collection coverage map:
	// a seed whose name is already in the collection (or is a combined
	// "A / B" name covered by standalone docs) must not be re-seeded.
	// The map is a snapshot of the database taken before this run, the
	// same contract the units and structures shelves use: on a green-field
	// database a combined seed and a standalone seed of the same part can
	// both land, and the next run skips the redundant re-upsert. The
	// lookup stays deterministic either way (the store returns the first
	// name-ordered exact match).
	knownDocs := map[string]map[string][]string{}
	for _, coll := range docCollections {
		k, err := loadKnown(coll)
		if err != nil {
			return err
		}
		knownDocs[coll] = k
	}
	var unitOps []mongo.WriteModel
	var structOps []mongo.WriteModel
	// docOps holds one upsert list per gear/world collection.
	docOps := map[string][]mongo.WriteModel{}
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
			if strings.TrimSpace(u.Name) == "" || covered(knownUnits, u.Name) {
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
			if strings.TrimSpace(s.Name) == "" || covered(knownStructures, s.Name) {
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
		shelves := d.shelves()
		for _, coll := range docCollections {
			for _, ds := range shelves[coll] {
				if ds.Name == "" || covered(knownDocs[coll], ds.Name) {
					continue
				}
				seed := seedDoc(ds, "data/"+filepath.Base(f))
				// No text_blob: the fuzzy ranker that read it is gone, and
				// findAll excludes it — seeds carry only curated fields.
				// No fetched_at: absent until the watcher writes a real
				// timestamp. An explicit null would sit beside strings in
				// the same field across docs.
				docOps[coll] = append(docOps[coll], mongo.NewUpdateOneModel().
					SetFilter(bson.M{"name": ds.Name}).
					SetUpdate(bson.M{"$setOnInsert": seed}).
					SetCollation(ciCollation).
					SetUpsert(true))
			}
		}
	}
	total := len(unitOps) + len(structOps)
	for _, ops := range docOps {
		total += len(ops)
	}
	if total == 0 {
		fmt.Println("ingest: nothing to do")
		return nil
	}
	// Fresh timeout for the write phase: the 60s ctx above already covered
	// indexes plus coverage loads, and a growing seed set must not deadline
	// the writes.
	wctx, wcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer wcancel()
	var upserted, matched int64
	// Unordered writes: one bad doc must not abort the rest of a 270-doc
	// shelf. Seed names are unique per collection (the seed-file test
	// enforces it), so two upserts can never race for the same doc.
	bulkOpts := options.BulkWrite().SetOrdered(false)
	if len(unitOps) > 0 {
		res, err := db.Collection("units").BulkWrite(wctx, unitOps, bulkOpts)
		if err != nil {
			return fmt.Errorf("seed units: %w", err)
		}
		upserted += res.UpsertedCount
		matched += res.MatchedCount
	}
	if len(structOps) > 0 {
		res, err := db.Collection("structures").BulkWrite(wctx, structOps, bulkOpts)
		if err != nil {
			return fmt.Errorf("seed structures: %w", err)
		}
		upserted += res.UpsertedCount
		matched += res.MatchedCount
	}
	for _, coll := range docCollections {
		ops := docOps[coll]
		if len(ops) == 0 {
			continue
		}
		res, err := db.Collection(coll).BulkWrite(wctx, ops, bulkOpts)
		if err != nil {
			return fmt.Errorf("seed %s: %w", coll, err)
		}
		upserted += res.UpsertedCount
		matched += res.MatchedCount
	}
	fmt.Printf("ingest: upserted=%d matched=%d\n", upserted, matched)
	return nil
}
