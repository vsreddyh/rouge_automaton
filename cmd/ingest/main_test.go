package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestDocSeedUnmarshal pins the free-form seed contract: name and aliases
// are lifted out, everything else becomes the doc body, and nested objects
// become BSON documents rather than opaque blobs.
func TestDocSeedUnmarshal(t *testing.T) {
	var d docSeed
	raw := `{
		"name": "  Orbital Railcannon Strike ",
		"aliases": ["Orbital Railcannon", 42],
		"role": "Support stratagem.",
		"params": {"damage": "230", "cooldown": "60 s"},
		"counters": ["one-shots a Hulk", "aim at the ground"]
	}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Name != "Orbital Railcannon Strike" {
		t.Errorf("name = %q, want trimmed", d.Name)
	}
	if len(d.Aliases) != 2 || d.Aliases[0] != "Orbital Railcannon" {
		t.Errorf("aliases = %v, want the string and the coerced number", d.Aliases)
	}
	if d.Aliases[1] != "42" {
		t.Errorf("alias 1 = %q, want the coerced number 42", d.Aliases[1])
	}
	if _, ok := d.Fields["name"]; ok {
		t.Error("name must not stay in the field bag")
	}
	if _, ok := d.Fields["aliases"]; ok {
		t.Error("aliases must not stay in the field bag")
	}
	params, ok := d.Fields["params"].(bson.M)
	if !ok {
		t.Fatalf("params = %T, want bson.M", d.Fields["params"])
	}
	if params["damage"] != "230" {
		t.Errorf("params.damage = %v, want 230", params["damage"])
	}
	counters, ok := d.Fields["counters"].(bson.A)
	if !ok {
		t.Fatalf("counters = %T, want bson.A", d.Fields["counters"])
	}
	if len(counters) != 2 || counters[0] != "one-shots a Hulk" {
		t.Errorf("counters = %v, want the two bullets", counters)
	}
}

// TestDocSeedNoName: a seed without a name is skipped by the ingest, so
// the parsed doc must be empty rather than half-populated.
func TestDocSeedNoName(t *testing.T) {
	var d docSeed
	if err := json.Unmarshal([]byte(`{"role": "no name here"}`), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Name != "" {
		t.Errorf("name = %q, want empty", d.Name)
	}
	if len(d.Aliases) != 0 {
		t.Errorf("aliases = %v, want empty", d.Aliases)
	}
}

// TestBsonifyNested pins the recursive conversion of nested seed objects.
func TestBsonifyNested(t *testing.T) {
	in := map[string]any{
		"flat": "value",
		"obj":  map[string]any{"inner": map[string]any{"deep": "1"}},
		"arr":  []any{"a", map[string]any{"b": "c"}},
	}
	out := bsonify(in)
	if out["flat"] != "value" {
		t.Errorf("flat = %v, want value", out["flat"])
	}
	obj, ok := out["obj"].(bson.M)
	if !ok {
		t.Fatalf("obj = %T, want bson.M", out["obj"])
	}
	if _, ok := obj["inner"].(bson.M); !ok {
		t.Errorf("inner = %T, want bson.M", obj["inner"])
	}
	arr, ok := out["arr"].(bson.A)
	if !ok {
		t.Fatalf("arr = %T, want bson.A", out["arr"])
	}
	if _, ok := arr[1].(bson.M); !ok {
		t.Errorf("arr[1] = %T, want bson.M", arr[1])
	}
}

// TestCovered pins the coverage rule: a combined "A / B" name is covered
// when a part exists as a standalone doc or alias. A doc never covers
// itself — its own aliases must not make it redundant — and a slash inside
// a designation is not a separator.
func TestCovered(t *testing.T) {
	known := map[string][]string{
		"charger / behemoth": {"charger"},
		"tundra":             {"snow"},
		"ar/gl-21 one-two":   {"one two"},
	}
	cases := []struct {
		seed string
		want bool
	}{
		{"Charger / Behemoth", false}, // itself: re-upserted, never re-inserted
		{"Tundra", false},             // itself
		{"TUNDRA", false},             // itself, case-insensitively
		{"Charger", true},             // alias of the combined doc
		{"Behemoth", false},           // only the combined doc holds it
		{"AR/GL-21 One-Two", false},   // itself: the slash is not a separator
		{"Snow", true},                // alias
		{"Brand New Item", false},     // nothing known
	}
	for _, c := range cases {
		if got := covered(known, c.seed); got != c.want {
			t.Errorf("covered(%q) = %v, want %v", c.seed, got, c.want)
		}
	}
	// A standalone doc makes the combined name redundant.
	withStandalone := map[string][]string{"charger": nil}
	if !covered(withStandalone, "Charger / Behemoth") {
		t.Error("a combined name must be covered when a part exists standalone")
	}
}

// TestSeedDocReservedFields: a seed bag must never carry the fields Mongo
// or the wiki watcher own. The bag is applied first, so the reserved
// fields are set by the ingest and the denylisted ones never arrive.
func TestSeedDocReservedFields(t *testing.T) {
	ds := docSeed{
		Name:    "Orbital Railcannon Strike",
		Aliases: []string{"Orbital Railcannon"},
		Fields: bson.M{
			"role":          "Support stratagem.",
			"_id":           "seed-should-not-own-this",
			"source":        "seed-should-not-own-this",
			"source_url":    "seed-should-not-own-this",
			"rev_id":        99,
			"patch_version": "9.9.9",
			"fetched_at":    "whenever",
			"text_blob":     "dead weight",
			"embedding":     []float32{0.1},
		},
	}
	seed := seedDoc(ds, "data/gear.json")
	// Denylisted fields never reach the doc.
	for _, reserved := range []string{"_id", "fetched_at", "text_blob", "embedding"} {
		if _, ok := seed[reserved]; ok {
			t.Errorf("seed must not carry %q", reserved)
		}
	}
	// The fields the ingest owns are set by the ingest, not the bag.
	if seed["source"] != "data/gear.json" {
		t.Errorf("source = %v, want data/gear.json", seed["source"])
	}
	if seed["source_url"] != "" {
		t.Errorf("source_url = %v, want empty", seed["source_url"])
	}
	if seed["rev_id"] != 0 {
		t.Errorf("rev_id = %v, want 0", seed["rev_id"])
	}
	if seed["patch_version"] != "" {
		t.Errorf("patch_version = %v, want empty", seed["patch_version"])
	}
	if seed["name"] != ds.Name {
		t.Errorf("name = %v, want %q", seed["name"], ds.Name)
	}
	if seed["role"] != "Support stratagem." {
		t.Errorf("role = %v, want the seed's own field", seed["role"])
	}
	if aliases, ok := seed["aliases"].([]string); !ok || len(aliases) != 1 {
		t.Errorf("aliases = %v, want the seed's own aliases", seed["aliases"])
	}
	// A seed without aliases leaves the field out: a nil slice would
	// encode as BSON null and sit beside arrays in the same field.
	plain := seedDoc(docSeed{Name: "Tundra"}, "data/world.json")
	if _, ok := plain["aliases"]; ok {
		t.Errorf("aliases must be absent, got %v", plain["aliases"])
	}
}

// TestStructureDocHasNoRole pins issue #53: a structure's note is tactical
// advice (a counter), so the seed never copies it into role. get_unit and
// FilterUnits already fall back to note.
func TestStructureDocHasNoRole(t *testing.T) {
	seed := structureDoc("automatons", "data/automatons.json", structureSeed{
		Name: "Fabricator", Note: "Destroy early.", Counters: []string{"Grenade"},
	})
	if _, ok := seed["role"]; ok {
		t.Fatalf("structure seed must not carry role, got %v", seed)
	}
	if seed["note"] != "Destroy early." {
		t.Errorf("note = %v, want the seed's note", seed["note"])
	}
	if seed["source"] != "data/automatons.json" || seed["source_url"] != "" {
		t.Errorf("provenance = %v/%v", seed["source"], seed["source_url"])
	}
}

// TestSeedFiles pins the shipped seed data: every file parses, every gear
// and world shelf is populated, names are unique per collection, and no
// doc's alias is another doc's name (an alias that shadows a name would
// make covered() skip that doc forever).
func TestSeedFiles(t *testing.T) {
	dir := filepath.Join("..", "..", "skills", "rouge-automaton", "data")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no seed files in %s: %v", dir, err)
	}
	// Names and aliases per collection across every file: a shelf can
	// span files, so uniqueness is a collection-wide property.
	shelvesSeen := map[string]bool{}
	names := map[string]map[string]string{}
	aliases := map[string]map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var d dataFile
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for coll, docs := range d.shelves() {
			if len(docs) == 0 {
				continue
			}
			shelvesSeen[coll] = true
			if names[coll] == nil {
				names[coll] = map[string]string{}
			}
			if aliases[coll] == nil {
				aliases[coll] = map[string]string{}
			}
			for _, doc := range docs {
				if doc.Name == "" {
					t.Errorf("%s: %s has a doc with no name", f, coll)
					continue
				}
				key := strings.ToLower(doc.Name)
				if prev, dup := names[coll][key]; dup {
					t.Errorf("%s: %s has duplicate names %q and %q", f, coll, prev, doc.Name)
				}
				names[coll][key] = doc.Name
				for _, a := range doc.Aliases {
					al := strings.ToLower(strings.TrimSpace(a))
					if al == "" {
						t.Errorf("%s: %s doc %q has an empty alias", f, coll, doc.Name)
						continue
					}
					if prev, dup := aliases[coll][al]; dup && prev != doc.Name {
						t.Errorf("%s: %s docs %q and %q both alias %q", f, coll, prev, doc.Name, a)
					}
					aliases[coll][al] = doc.Name
				}
			}
		}
	}
	// An alias that shadows another doc's name wins the exact lookup and
	// makes covered() skip that doc forever.
	for coll, byName := range names {
		for al, owner := range aliases[coll] {
			if other, clash := byName[al]; clash && other != owner {
				t.Errorf("%s: doc %q aliases %q, which is doc %q", coll, owner, al, other)
			}
		}
	}
	for _, coll := range docCollections {
		if !shelvesSeen[coll] {
			t.Errorf("no seed file populates the %s shelf", coll)
		}
	}
}
