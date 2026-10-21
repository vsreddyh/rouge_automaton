package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const sampleWeapons = `{
  "meta": {"dataVersion": "2026-10-20.abc", "count": 2},
  "data": [
    {"id": "ar-11-arbitrator", "name": "AR-11 Arbitrator", "upcoming": false,
     "aliases": [], "wiki": {"title": "AR-11 Arbitrator", "url": "https://helldivers.wiki.gg/wiki/AR-11_Arbitrator", "flags": ["potentially_outdated"]},
     "category": "primary", "subcategory": "assault_rifle", "traitIds": ["light-armor-penetrating"],
     "firearm": {"dps": [1400, 600], "fireRateRpm": [1200]},
     "statsRaw": {"DPS": "1,400 (4mm)600 (10g)", "Fire Rate": "1,200 rpm"},
     "source": {"type": "warbond", "label": "Ironclad Democracy P1", "page": 1}},
    {"id": "g-11-caltrops", "name": "G-11 Caltrops", "upcoming": true,
     "wiki": {"title": "G-11 Caltrops", "url": "https://helldivers.wiki.gg/wiki/G-11_Caltrops"},
     "category": "throwable", "subcategory": "special"}
  ]
}`

func TestParse(t *testing.T) {
	snap, err := Parse([]byte(sampleWeapons))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != "2026-10-20.abc" || snap.Count() != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	it := snap.Items[0]
	if it.Category != "primary" || it.Subcategory != "assault_rifle" {
		t.Fatalf("item = %+v", it)
	}
	if it.Wiki.URL != "https://helldivers.wiki.gg/wiki/AR-11_Arbitrator" {
		t.Fatalf("provenance = %+v", it.Wiki)
	}
	if !snap.Items[1].Upcoming {
		t.Fatal("the unreleased item must be flagged")
	}
}

// The warbonds file reports superCreditsClaimable as a number upstream,
// not a bool: the context collection must still parse.
func TestParseWarbondsNumericClaim(t *testing.T) {
	raw := `{"meta": {"dataVersion": "2026-10-21.xyz", "count": 3}, "data": [
	  {"id": "wb-1", "name": "Warbond One", "type": "warbond", "superCreditsClaimable": 300},
	  {"id": "wb-2", "name": "Warbond Two", "type": "warbond", "superCreditsClaimable": true},
	  {"id": "wb-3", "name": "Warbond Three", "type": "warbond"}
	]}`
	snap, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count() != 3 {
		t.Fatalf("count = %d, want 3", snap.Count())
	}
	if got, want := snap.Items[0].SuperCreditsClaim, 300.0; got != want {
		t.Fatalf("items[0].SuperCreditsClaim = %#v, want %#v", got, want)
	}
	if got, want := snap.Items[1].SuperCreditsClaim, true; got != want {
		t.Fatalf("items[1].SuperCreditsClaim = %#v, want %#v", got, want)
	}
	if snap.Items[2].SuperCreditsClaim != nil {
		t.Fatalf("items[2].SuperCreditsClaim = %#v, want nil", snap.Items[2].SuperCreditsClaim)
	}
}

func TestShelfForCollection(t *testing.T) {
	for coll, want := range map[string]string{
		"weapons": "weapons", "stratagems": "stratagems", "boosters": "boosters",
		"armors": "armor", "helmets": "armor", "armor-sets": "armor",
		"passives": "", "warbonds": "", "weapon-traits": "",
	} {
		if got := ShelfForCollection(coll); got != want {
			t.Fatalf("ShelfForCollection(%q) = %q, want %q", coll, got, want)
		}
	}
}

// An unchanged snapshot costs one 304: the ETag the catalog hands out is
// sent back, and the payload never comes down twice.
func TestFetchNotModified(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("If-None-Match")
		if got == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(sampleWeapons))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	res, err := c.Fetch(context.Background(), "weapons", "")
	if err != nil || res.NotModified {
		t.Fatalf("first fetch = %+v err=%v", res, err)
	}
	if res.ETag != `"v1"` || res.Snapshot.Count() != 2 {
		t.Fatalf("first fetch = %+v", res)
	}
	res2, err := c.Fetch(context.Background(), "weapons", res.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.NotModified {
		t.Fatal("the second fetch must be a 304")
	}
	if got != `"v1"` {
		t.Fatalf("If-None-Match = %q", got)
	}
}

// The catalog's query and search endpoints are the rate-limited ones; the
// sync must only ever read the static files.
func TestURLIsStatic(t *testing.T) {
	c := &Client{Base: "https://example.test"}
	u := c.URL("weapons")
	if !strings.HasSuffix(u, "/v1/weapons.json") {
		t.Fatalf("url = %q", u)
	}
	if strings.Contains(u, "query") || strings.Contains(u, "search") {
		t.Fatalf("the sync must not touch the rate-limited endpoints: %q", u)
	}
}

func TestMergeItems(t *testing.T) {
	armor := Item{ID: "a-35-recon", Name: "A-35 Recon", Wiki: WikiRef{Title: "A-35 Recon", URL: "u"}, Weight: "medium"}
	helmet := Item{ID: "a-35-recon", Name: "A-35 Recon", Wiki: WikiRef{Title: "A-35 Recon", URL: "u"}, Description: "A helmet."}
	merged := mergeItems([]Item{armor, helmet})
	if len(merged) != 1 {
		t.Fatalf("merged = %+v", merged)
	}
	if merged[0].Weight != "medium" || merged[0].Description != "A helmet." {
		t.Fatalf("merged = %+v", merged[0])
	}
}

// The curated seed wins; the catalog's different number is reported.
func TestDiffCuratedReportsDisagreement(t *testing.T) {
	doc := bson.M{"params": bson.M{"dps": "1,400 (4mm)600 (10g)"}}
	it := Item{Firearm: map[string]any{"dps": []any{1400.0, 600.0}}}
	if diff := diffCurated(doc, it); len(diff) != 0 {
		t.Fatalf("the seed carries both modes: %+v", diff)
	}
	doc2 := bson.M{"params": bson.M{"dps": "960"}}
	if diff := diffCurated(doc2, it); len(diff) != 1 {
		t.Fatalf("diff = %+v, want one disagreement", diff)
	}
}

func TestDiffCuratedArmorNumbers(t *testing.T) {
	doc := bson.M{"params": bson.M{"armor": "50", "speed": "550"}}
	it := Item{ArmorRating: 100.0, Speed: 500.0}
	diff := diffCurated(doc, it)
	if len(diff) != 2 {
		t.Fatalf("diff = %+v, want two disagreements", diff)
	}
	for _, d := range diff {
		if d.Curated == "" || d.Catalog == "" {
			t.Fatalf("mismatch must carry both sides: %+v", d)
		}
	}
}

// The seed rounds the weapon numbers; the catalog carries them exactly.
// The difference is reported, never applied.
func TestDiffCuratedReportsWeaponNumbers(t *testing.T) {
	doc := bson.M{"params": bson.M{"recoil": "13", "capacity": "45"}}
	it := Item{Firearm: map[string]any{"recoil": 13.25, "capacity": 45.0}}
	diff := diffCurated(doc, it)
	if len(diff) != 1 {
		t.Fatalf("diff = %+v, want the rounded recoil only", diff)
	}
	if diff[0].Field != "params.recoil" {
		t.Fatalf("field = %q", diff[0].Field)
	}
	if diff[0].Curated != "13" || diff[0].Catalog != "13.25" {
		t.Fatalf("mismatch = %+v", diff[0])
	}
}

// An alias the catalog knows and the seed does not, and a role the seed
// never wrote: both are gaps the curator should see.
func TestDiffCuratedReportsAliasAndRoleGaps(t *testing.T) {
	doc := bson.M{"name": "Hover Pack", "aliases": bson.A{"LIFT-860"}}
	it := Item{Description: "The Hover Pack is a backpack.", Aliases: []string{"LIFT-860 Hover Pack"}}
	diff := diffCurated(doc, it)
	fields := map[string]bool{}
	for _, d := range diff {
		fields[d.Field] = true
	}
	if !fields["aliases"] || !fields["role"] {
		t.Fatalf("diff = %+v, want the alias and the role gap", diff)
	}
	// A curated role the catalog does not contradict is not a gap.
	filled := bson.M{"name": "Hover Pack", "role": "A backpack."}
	if diff := diffCurated(filled, it); len(diff) != 1 || diff[0].Field != "aliases" {
		t.Fatalf("diff = %+v, want the alias gap only", diff)
	}
}

func TestParamsFromStats(t *testing.T) {
	params := paramsFromStats(map[string]any{"Standard Damage": "70 Ballistic", "Fire Rate": "1,200 rpm"})
	if params["standard_damage"] != "70 Ballistic" || params["fire_rate"] != "1,200 rpm" {
		t.Fatalf("params = %+v", params)
	}
}

func TestSourceLabel(t *testing.T) {
	it := Item{Source: map[string]any{"type": "warbond", "label": "Ironclad Democracy P1"}}
	if got := sourceLabel(it); got != "Ironclad Democracy P1" {
		t.Fatalf("label = %q", got)
	}
	if got := sourceLabel(Item{}); got != "" {
		t.Fatalf("label = %q, want empty", got)
	}
}
