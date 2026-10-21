package wikiwatch

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rouge_automaton/internal/wikifeed"
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

// The wiki's own categories decide the shelf. The type category decides —
// the generic "Weapons" category sits on primaries, secondaries,
// throwables and the support-weapon stratagem, so it decides nothing.
func TestShelfForCategories(t *testing.T) {
	cases := []struct {
		cats []string
		want string
	}{
		{[]string{"Enemies", "Automatons", "Large Enemies"}, "units"},
		{[]string{"Enemies", "Illuminate", "Small Enemies"}, "units"},
		{[]string{"Armor", "Light Armor", "Helmets"}, "armor"},
		{[]string{"Primary Weapons", "Assault Rifles"}, "weapons"},
		{[]string{"Secondary Weapons", "Pistols"}, "weapons"},
		{[]string{"Throwables", "Standard Throwables"}, "weapons"},
		{[]string{"Stratagems", "Support Weapon Stratagems", "Support Weapons", "Weapons"}, "stratagems"},
		{[]string{"Sentry Stratagems"}, "stratagems"},
		{[]string{"Weapons"}, ""},
		{[]string{"Guides", "Patch Notes"}, ""},
		{[]string{"Biomes"}, "biomes"},
		{[]string{"Planets"}, "planets"},
		{[]string{"Structures", "Automatons"}, "structures"},
	}
	for _, tc := range cases {
		if got := ShelfForCategories(tc.cats); got != tc.want {
			t.Fatalf("ShelfForCategories(%v) = %q, want %q", tc.cats, got, tc.want)
		}
	}
}

func TestFactionOf(t *testing.T) {
	if got := FactionOf([]string{"Enemies", "Terminids"}); got != "terminids" {
		t.Fatalf("faction = %q", got)
	}
	if got := FactionOf([]string{"Enemies"}); got != "" {
		t.Fatalf("faction = %q, want empty", got)
	}
}

func TestTitleFromURL(t *testing.T) {
	if got := titleFromURL("https://helldivers.wiki.gg/wiki/AR-11_Arbitrator"); got != "AR-11 Arbitrator" {
		t.Fatalf("title = %q", got)
	}
	if got := titleFromURL(""); got != "" {
		t.Fatalf("title = %q, want empty", got)
	}
}

// The feed keeps carrying the pages that match no shelf; re-checking them
// would spend a wiki request on every poll for nothing.
func TestQueueRemembersIgnoredPages(t *testing.T) {
	w := New(nil, nil)
	w.queue("Patch Notes 1.1")
	if len(w.untracked) != 1 {
		t.Fatalf("queue = %v", w.untracked)
	}
	w.queue("Patch Notes 1.1")
	if len(w.untracked) != 1 {
		t.Fatalf("the same page must be queued once: %v", w.untracked)
	}
	w.ignore("Patch Notes 1.1")
	w.untracked = nil
	w.queue("Patch Notes 1.1")
	if len(w.untracked) != 0 {
		t.Fatalf("an ignored page must not be queued again: %v", w.untracked)
	}
}

// A curated params bag is the seed's work: the wiki infobox must not
// overwrite it when a page is refreshed.
func TestBuildFieldsKeepsCuratedParams(t *testing.T) {
	w := New(nil, nil)
	cur := bson.M{"name": "AR-11 Arbitrator", "params": bson.M{"dps": "1,400 (4mm)600 (10g)"}}
	page := wikifeed.Page{RevID: 9, Timestamp: "2026-10-01T00:00:00Z",
		Wikitext: "{{Infobox Weapon\n| damage = 70\n| dps = 1,400\n}}"}
	set := w.buildFields("weapons", page, cur, "https://helldivers.wiki.gg/wiki/AR-11_Arbitrator")
	if _, ok := set["params"]; ok {
		t.Fatalf("the wiki infobox must not overwrite the curated params: %v", set["params"])
	}
	if set["wiki_title"] != "AR-11 Arbitrator" {
		t.Fatalf("wiki_title = %v", set["wiki_title"])
	}
}

// A doc with no curated params yet takes the wiki's infobox.
func TestBuildFieldsFillsEmptyParams(t *testing.T) {
	w := New(nil, nil)
	page := wikifeed.Page{RevID: 9, Wikitext: "{{Infobox Weapon\n| damage = 70\n}}"}
	set := w.buildFields("weapons", page, bson.M{"name": "New Gun"}, "https://x/wiki/New_Gun")
	params, ok := set["params"].(bson.M)
	if !ok || params["damage"] != "70" {
		t.Fatalf("params = %v", set["params"])
	}
}
