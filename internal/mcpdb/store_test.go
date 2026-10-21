package mcpdb

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestValuesShapes pins the single shape-switch behind strList and
// ingest's loadKnown: every writer shape coerces identically.
func TestValuesShapes(t *testing.T) {
	cases := []struct {
		in   any
		want []string
	}{
		{bson.A{"a", 1}, []string{"a", "1"}},
		{[]any{"b"}, []string{"b"}},
		{[]string{"c"}, []string{"c"}},
		{"d", []string{"d"}},
		{42, []string{"42"}},
		{nil, nil},
		{bson.D{{Key: "k", Value: "v"}}, []string{"v"}},
		{[]bson.D{{{Key: "k", Value: "v"}}}, []string{"v"}},
		{bson.M{"k": "v"}, []string{"v"}},
		{map[string]any{"k": "v"}, []string{"v"}},
		{[]bson.M{{"k": "v"}}, []string{"v"}},
		{[]map[string]any{{"k": "v"}}, []string{"v"}},
	}
	for _, tc := range cases {
		if got, want := strings.Join(Values(tc.in), ","), strings.Join(tc.want, ","); got != want {
			t.Fatalf("Values(%v) = %q, want %q", tc.in, got, want)
		}
	}
}

// The router is gone by design (the model decides what to look up), so the
// tests pin the two remaining contracts: the faction/category allowlists and
// exact alias-resolved matching.
func TestFactions(t *testing.T) {
	for _, f := range []string{"automatons", "terminids", "illuminate"} {
		if !Factions[f] {
			t.Fatalf("%q must be a valid faction", f)
		}
	}
	for _, f := range []string{"bots", "squids", ""} {
		if Factions[f] {
			t.Fatalf("%q must not be a valid faction", f)
		}
	}
}

func TestGearCategories(t *testing.T) {
	for _, c := range []string{"weapons", "stratagems", "armor", "boosters"} {
		if !GearCategories[c] {
			t.Fatalf("%q must be a valid gear category", c)
		}
	}
	for _, c := range []string{"gear", "units", ""} {
		if GearCategories[c] {
			t.Fatalf("%q must not be a valid gear category", c)
		}
	}
}

func TestFindExact(t *testing.T) {
	hulk := bson.M{"name": "Hulk Bruiser", "aliases": bson.A{"hulk"},
		"text_blob": "Hulk Bruiser | automatons | Massive walker"}
	other := bson.M{"name": "Agitator", "aliases": bson.A{"radical"},
		"text_blob": "Agitator | automatons | how to kill the radical"}
	docs := []bson.M{hulk, other}
	for _, q := range []string{"Hulk Bruiser", "hulk", " HULK ", "Hulk bruiser"} {
		got, ok := FindExact(docs, q)
		if !ok || docName(got) != "Hulk Bruiser" {
			t.Fatalf("FindExact(%q) must hit Hulk Bruiser, got %v ok=%v", q, got, ok)
		}
	}
	// Alias of another doc must not shadow the canonical hit.
	if got, ok := FindExact(docs, "radical"); !ok || docName(got) != "Agitator" {
		t.Fatalf("alias lookup must hit Agitator, got %v ok=%v", got, ok)
	}
	// Substrings and noise words never match: exact tools do not rank.
	for _, q := range []string{"", "hulk bruiser extra", "best loadout vs Automatons", "hulk bru"} {
		if _, ok := FindExact(docs, q); ok {
			t.Fatalf("FindExact(%q) must miss", q)
		}
	}
	// Missing name falls back to "?" and never matches a real query.
	bare := bson.M{"aliases": bson.A{"mystery"}}
	if _, ok := FindExact([]bson.M{bare}, "mystery"); !ok {
		t.Fatal("alias-only doc must match on its alias")
	}
	// Document-shaped and scalar aliases coerce like ingest's loadKnown.
	dDoc := bson.M{"name": "Hulk", "aliases": bson.D{{Key: "x", Value: "hulk"}}}
	if _, ok := FindExact([]bson.M{dDoc}, "hulk"); !ok {
		t.Fatal("bson.D aliases must match")
	}
	arrDoc := bson.M{"name": "Hulk", "aliases": []bson.D{{{Key: "x", Value: "hulk"}}}}
	if _, ok := FindExact([]bson.M{arrDoc}, "hulk"); !ok {
		t.Fatal("[]bson.D aliases must match")
	}
	// Map-shaped aliases contribute their values.
	mapDoc := bson.M{"name": "Hulk", "aliases": bson.M{"x": "hulk"}}
	if _, ok := FindExact([]bson.M{mapDoc}, "hulk"); !ok {
		t.Fatal("bson.M aliases must match")
	}
	if _, ok := FindExact([]bson.M{bson.M{"name": "Mystery"}}, ""); ok {
		t.Fatal("empty query must never match")
	}
}

func TestFilterUnits(t *testing.T) {
	docs := []bson.M{
		{"name": "Hulk Bruiser", "faction": "automatons", "role": "Massive walker"},
		{"name": "Trooper", "faction": "automatons", "role": "Basic infantry"},
		{"name": "Charger", "faction": "terminids", "role": "Armored linebreaker"},
	}
	got := FilterUnits(docs, "automatons", "")
	if len(got) != 2 {
		t.Fatalf("faction filter must keep 2, got %v", got)
	}
	got = FilterUnits(docs, "AUTOMATONS", "walker")
	if len(got) != 1 || docName(got[0]) != "Hulk Bruiser" {
		t.Fatalf("role substring must narrow to Hulk, got %v", got)
	}
	if got := FilterUnits(docs, "bots", ""); len(got) != 0 {
		t.Fatalf("unknown faction must yield no docs, got %v", got)
	}
}

func TestFormatFull(t *testing.T) {
	doc := bson.M{
		"name": "Hulk Bruiser", "faction": "automatons", "role": "Massive walker",
		"weak_points": bson.A{"Rear vent"}, "counters": bson.A{"AMR"},
		"health": 1800, "size": "Massive", "source": "data/automatons.json",
		"anatomy":      bson.A{bson.M{"part": "Vent", "av": "0", "fatal": "yes"}},
		"wiki_tactics": []string{"Flank it"},
	}
	got := FormatFull(doc)
	for _, want := range []string{"Hulk Bruiser", "Rear vent", "AMR", "health 1800", "Vent", "Flank it", "source:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatFull must carry %q, got %q", want, got)
		}
	}
	// Gear-shaped docs render through the effect/procurement fallbacks.
	gear := bson.M{"name": "Railcannon", "effect_text": "Big boom", "procurement": "Ship module"}
	if got := FormatFull(gear); !strings.Contains(got, "Big boom") || !strings.Contains(got, "Ship module") {
		t.Fatalf("gear fallbacks must render, got %q", got)
	}
	// Non-string values coerce instead of vanishing (review r2).
	numGear := bson.M{"name": "Mortar", "params": bson.M{"damage": 120, "active": true}}
	if got := FormatFull(numGear); !strings.Contains(got, "damage=120") || !strings.Contains(got, "active=true") {
		t.Fatalf("numeric params must coerce, got %q", got)
	}
	numAnatomy := bson.M{"name": "Hulk", "anatomy": bson.A{bson.M{"part": "Vent", "av": 0, "fatal": 1}}}
	if got := FormatFull(numAnatomy); !strings.Contains(got, "AV:0") || !strings.Contains(got, "fatal:1") {
		t.Fatalf("numeric anatomy must coerce, got %q", got)
	}
	// Caller-constructed slices and zero/negative health (review r5).
	rawAnatomy := bson.M{"name": "Hulk", "anatomy": []bson.M{{"part": "Vent"}}}
	if got := FormatFull(rawAnatomy); !strings.Contains(got, "Vent") {
		t.Fatalf("[]bson.M anatomy must render, got %q", got)
	}
	for _, h := range []any{"0.0", "00", -5, "n/a", true} {
		if got := FormatFull(bson.M{"name": "X", "health": h}); strings.Contains(got, "health") {
			t.Fatalf("non-positive health %v must not render, got %q", h, got)
		}
	}
	if got := FormatFull(bson.M{"name": "X", "health": "1,800"}); !strings.Contains(got, "health 1,800") {
		t.Fatalf("comma-grouped health must pass through, got %q", got)
	}
	if got := FormatFull(bson.M{"name": "X", "health": " 100 "}); !strings.Contains(got, "health 100") {
		t.Fatalf("padded health must trim and render, got %q", got)
	}
	rawMapAnatomy := bson.M{"name": "Hulk", "anatomy": []map[string]any{{"part": "Vent"}}}
	if got := FormatFull(rawMapAnatomy); !strings.Contains(got, "Vent") {
		t.Fatalf("[]map[string]any anatomy must render, got %q", got)
	}
	rawDocAnatomy := bson.M{"name": "Hulk", "anatomy": []bson.D{{{Key: "part", Value: "Vent"}}}}
	if got := FormatFull(rawDocAnatomy); !strings.Contains(got, "Vent") {
		t.Fatalf("[]bson.D anatomy must render, got %q", got)
	}
	// Numeric provenance coerces instead of falling back to mongo.
	numSrc := bson.M{"name": "X", "source": 123}
	if got := FormatFull(numSrc); !strings.Contains(got, "[source: 123]") {
		t.Fatalf("numeric source must render, got %q", got)
	}
	// Brackets inside values must not break the [source:] citation contract.
	brk := bson.M{"name": "X", "faction": "bots]", "source": "a]b"}
	if got := FormatFull(brk); strings.Contains(got, "bots]") || strings.Contains(got, "a]b") ||
		!strings.Contains(got, "[source: a b]") {
		t.Fatalf("brackets must not leak into markers, got %q", got)
	}
	// Newline keys flatten too — a wiki infobox key must never forge a
	// second cited line (review r4).
	nlKey := bson.M{"name": "Mortar", "params": bson.M{"da\nmage": 120}}
	if got := FormatFull(nlKey); strings.Contains(got, "\n") || !strings.Contains(got, "da mage=120") {
		t.Fatalf("newline param keys must flatten, got %q", got)
	}
	// Embedded newlines must never forge a second cited line (review r3).
	multi := bson.M{"name": "Bog\nHulk", "role": "line1\nline2",
		"weak_points": bson.A{"rear\nvent"}, "counters": bson.A{"amr"},
		"source": "seed\nfile"}
	if got := FormatFull(multi); strings.Contains(got, "\n") {
		t.Fatalf("FormatFull must stay single-line, got %q", got)
	}
}

// TestFormatUnitRoster pins the issue-53 roster contract: a list line
// carries the name, the curated role and the source — never weak points or
// counters, so a roster answer cannot volunteer intel the user did not ask
// for.
func TestFormatUnitRoster(t *testing.T) {
	trooper := bson.M{
		"name": "Trooper", "faction": "automatons",
		"role": "Basic infantry.", "weak_points": bson.A{"Head"},
		"counters": bson.A{"Any primary"}, "source": "data/automatons.json",
	}
	got := FormatUnitRoster([]bson.M{trooper})
	want := "- Trooper: Basic infantry. [source: data/automatons.json]"
	if got != want {
		t.Fatalf("unit roster line = %q, want %q", got, want)
	}
	for _, banned := range []string{"weak", "counter", "Head", "Any primary"} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(banned)) {
			t.Fatalf("roster must not volunteer %q, got %q", banned, got)
		}
	}
	// A structure seeded before #53 copied note into role (possibly with
	// whitespace drift); that note is a tactic (a counter), so the roster
	// drops it and keeps the name.
	structure := bson.M{
		"name": "Fabricator", "note": "Destroy early, don't waste heavies.",
		"role": "Destroy early, don't waste heavies. ", "counters": bson.A{"Grenade"},
		"source": "data/automatons.json",
	}
	got = FormatUnitRoster([]bson.M{structure})
	if strings.Contains(got, "Destroy early") {
		t.Fatalf("note-as-role must not reach the roster, got %q", got)
	}
	if !strings.Contains(got, "- Fabricator [source:") {
		t.Fatalf("name-only structure line missing, got %q", got)
	}
	// A doc the watcher created has no curated role yet: name + source is
	// the whole line, never a dangling separator.
	if got := FormatUnitRoster([]bson.M{{"name": "New Unit", "source": "x"}}); got != "- New Unit [source: x]" {
		t.Fatalf("role-less doc = %q", got)
	}
	// One flat line per doc, even when a field carries a newline.
	multi := bson.M{"name": "Bog\nHulk", "role": "line1\nline2", "source": "seed\nfile"}
	if got := FormatUnitRoster([]bson.M{multi}); strings.Contains(got, "\n") {
		t.Fatalf("roster must stay one line per doc, got %q", got)
	}
	// Long roles cap so one wiki-grown role cannot blow a page.
	long := bson.M{"name": "X", "role": strings.Repeat("x", 500)}
	if got := FormatUnitRoster([]bson.M{long}); strings.Contains(got, strings.Repeat("x", rosterFieldCap+1)) {
		t.Fatalf("roster descriptor must cap at %d runes, got len %d", rosterFieldCap, len(got))
	}
	// A pathological watcher name caps too, so one doc cannot blow the page.
	longName := bson.M{"name": strings.Repeat("N", 500)}
	if got := FormatUnitRoster([]bson.M{longName}); strings.Contains(got, strings.Repeat("N", rosterFieldCap+1)) {
		t.Fatalf("roster name must cap at %d runes, got len %d", rosterFieldCap, len(got))
	}
}

// TestFormatGearRoster pins the gear half: gear role prose is a paragraph,
// so the roster carries the catalog subcategory instead (until the sync
// writes one, just the name), plus the source.
func TestFormatGearRoster(t *testing.T) {
	gear := bson.M{
		"name": "AR-23 Liberator", "role": "The AR-23 Liberator is a Primary Assault Rifle, available for all Helldivers.",
		"subcategory": "assault_rifle", "counters": bson.A{"one-shots a Hulk"},
		"source": "data/gear.json",
	}
	got := FormatGearRoster([]bson.M{gear})
	if got != "- AR-23 Liberator: assault rifle [source: data/gear.json]" {
		t.Fatalf("gear roster line = %q", got)
	}
	for _, banned := range []string{"Primary Assault Rifle", "counter"} {
		if strings.Contains(got, banned) {
			t.Fatalf("gear roster must not carry %q, got %q", banned, got)
		}
	}
	// Seeded docs carry no subcategory until #58 syncs the catalog.
	if got := FormatGearRoster([]bson.M{{"name": "Railcannon", "role": "Support stratagem."}}); got != "- Railcannon [source: mongo]" {
		t.Fatalf("subcategory-less gear line = %q", got)
	}
	// A multi-valued subcategory joins instead of vanishing.
	if got := FormatGearRoster([]bson.M{{"name": "X", "subcategory": bson.A{"assault_rifle"}}}); !strings.Contains(got, "assault rifle") {
		t.Fatalf("array subcategory must render, got %q", got)
	}
	// source_url wins over the seed file, the same citation contract as
	// FormatFull.
	url := bson.M{"name": "AR-23 Liberator", "source_url": "https://helldivers.wiki.gg/wiki/AR-23_Liberator", "source": "data/gear.json"}
	if got := FormatGearRoster([]bson.M{url}); !strings.Contains(got, "https://helldivers.wiki.gg/wiki/AR-23_Liberator") {
		t.Fatalf("roster must cite source_url, got %q", got)
	}
}

func TestFormatRosterEmpty(t *testing.T) {
	if got := FormatUnitRoster(nil); got != "" {
		t.Fatalf("empty roster = %q", got)
	}
	if got := FormatGearRoster(nil); got != "" {
		t.Fatalf("empty gear roster = %q", got)
	}
}

func TestClampLimit(t *testing.T) {
	if got := clampLimit(0); got != defaultListLimit {
		t.Fatalf("zero must default, got %d", got)
	}
	if got := clampLimit(999); got != maxListLimit {
		t.Fatalf("overlarge must cap, got %d", got)
	}
	if got := clampLimit(5); got != 5 {
		t.Fatalf("in-range must pass through, got %d", got)
	}
}

func TestPaginate(t *testing.T) {
	docs := []bson.M{{"name": "b"}, {"name": "a"}, {"name": "c"}}
	byName(docs)
	if docName(docs[0]) != "a" || docName(docs[1]) != "b" || docName(docs[2]) != "c" {
		t.Fatalf("byName must sort case-insensitively, got %v", docs)
	}
	// Offset keeps names beyond the first page discoverable.
	page := paginate(docs, 2, 2)
	if len(page) != 1 || docName(page[0]) != "c" {
		t.Fatalf("second page must hold c, got %v", page)
	}
	if got := paginate(docs, 2, 9); len(got) != 0 {
		t.Fatalf("past-end offset must yield no docs, got %v", got)
	}
	if got := clampOffset(-3); got != 0 {
		t.Fatalf("negative offset must floor at zero, got %d", got)
	}
}

// The catalog flags unreleased patch content: the doc is recorded, but a
// public bot must never serve it.
func TestUpcomingStaysOutOfAnswers(t *testing.T) {
	upcoming := bson.M{"name": "G-11 Caltrops", "upcoming": true}
	live := bson.M{"name": "AR-11 Arbitrator"}
	if !isUpcoming(upcoming) {
		t.Fatal("an upcoming doc must be recognised")
	}
	if isUpcoming(live) {
		t.Fatal("a live doc must not be treated as upcoming")
	}
	got := liveDocs([]bson.M{upcoming, live})
	if len(got) != 1 || docName(got[0]) != "AR-11 Arbitrator" {
		t.Fatalf("live docs = %v", got)
	}
}

// The way-out guard names an unreleased item without its stats. The name
// is model-supplied, so the line must carry no forged citation marker
// and stay capped like every other rendered roster value.
func TestUpcomingLineSanitized(t *testing.T) {
	got := upcomingLine("G-11 Caltrops [source: evil]")
	if strings.Contains(got, "[source:") || strings.Contains(got, "]") {
		t.Fatalf("upcoming line must not carry brackets: %q", got)
	}
	if !strings.Contains(got, "G-11 Caltrops") || !strings.Contains(got, "not in this build yet") {
		t.Fatalf("upcoming line must name the item and withhold stats: %q", got)
	}
	long := upcomingLine(strings.Repeat("x", 500))
	if len([]rune(long)) > rosterFieldCap+len(" is not in this build yet — unreleased patch content, stats withheld; report it as unreleased, never guess its stats") {
		t.Fatalf("upcoming line must cap a long name at %d runes: %q", rosterFieldCap, long)
	}
}
