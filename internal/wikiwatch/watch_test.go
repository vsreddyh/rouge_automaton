package wikiwatch

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/vsreddyh/rouge_automaton/internal/wikifeed"
)

func TestTrackedFrom(t *testing.T) {
	tr := trackedFrom(bson.M{"title": "Hulk Bruiser", "collection": "units", "rev_id": float64(135127)})
	if tr.Title != "Hulk Bruiser" || tr.Collection != "units" || tr.RevID != 135127 {
		t.Fatalf("tracked = %+v", tr)
	}
	// int32/int64 variants (BSON decodes to int32/int64 depending on driver).
	if got := trackedFrom(bson.M{"rev_id": int32(7)}).RevID; got != 7 {
		t.Fatalf("int32 rev = %d", got)
	}
	if got := trackedFrom(bson.M{"rev_id": int64(9)}).RevID; got != 9 {
		t.Fatalf("int64 rev = %d", got)
	}
}

func TestBuildFieldsUnitsPreservesCurated(t *testing.T) {
	w := New(nil, nil)
	cur := bson.M{
		"name": "Hulk Bruiser", "faction": "automatons",
		"aliases":     bson.A{"hulk"},
		"role":        "Massive walker",
		"weak_points": bson.A{"Back vent"},
		"counters":    bson.A{"AMR"},
	}
	page := wikifeed.Page{RevID: 42, Timestamp: "2026-09-14T00:00:00Z", Extract: "Big walker.",
		Wikitext: `{{Infobox Enemy
| size = 2
| health = 1,800
| min_difficulty = {{Difficulty|4}}
| damage_type = {{Damage|Explosion}}
}}
== Tactical Information ==
* Flank to the rear vent`}
	set := w.buildFields("units", page, cur, "https://x/wiki/Hulk_Bruiser")
	if set["size"] != "Large" || set["health"] != 1800 || set["min_difficulty"] != "Challenging (4)" {
		t.Fatalf("fields = %v", set)
	}
	if len(set["anatomy"].(bson.A)) != 0 {
		t.Fatal("no anatomy rows expected")
	}
	blob, _ := set["text_blob"].(string)
	for _, want := range []string{"Hulk Bruiser", "automatons", "Back vent", "AMR", "Flank to the rear vent"} {
		if !strings.Contains(blob, want) {
			t.Fatalf("blob missing %q: %s", want, blob)
		}
	}
}

func TestBuildFieldsBiomeNote(t *testing.T) {
	w := New(nil, nil)
	page := wikifeed.Page{RevID: 1, Extract: "A desert.",
		Wikitext: "{{Infobox Biome\n | archetype = Sandy\n}}\n== Environmental Conditions ==\nThis biome has no environmental conditions.\n"}
	set := w.buildFields("biomes", page, bson.M{"name": "Tundra"}, "https://x/wiki/Tundra")
	if set["archetype"] != "Sandy" || set["note"] != "No environmental conditions specific to this biome." {
		t.Fatalf("biome fields = %v", set)
	}
}

func TestCleanParams(t *testing.T) {
	in := map[string]string{
		"damage": "35", "image": "AR-23.png", "landscape": "x.png",
		"description": strings.Repeat("a", 300), "traits": "Light Armor Penetrating",
	}
	out := cleanParams(in)
	if _, ok := out["image"]; ok {
		t.Fatal("image must be dropped")
	}
	if _, ok := out["landscape"]; ok {
		t.Fatal("landscape must be dropped")
	}
	if _, ok := out["description"]; ok {
		t.Fatal("over-long values must be dropped")
	}
	if out["damage"] != "35" || out["traits"] == "" {
		t.Fatalf("useful params dropped: %v", out)
	}
}

func TestCutRunes(t *testing.T) {
	if got := cut("héllo—world", 5); got != "héllo" {
		t.Fatalf("cut = %q", got)
	}
}
