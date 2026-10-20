// Package mcpdb exposes the intel store as model tools. It is the single
// implementation behind two fronts: the Discord bot's think-act loop (which
// calls Execute directly) and the standalone MCP server (which serves the
// same tools over stdio). One mapping, no duplication, no router.
package mcpdb

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Factions is the closed set of valid faction filters. The model supplies
// these, so anything outside the set is rejected rather than guessed.
var Factions = map[string]bool{"automatons": true, "terminids": true, "illuminate": true}

// GearCategories is the closed set of valid gear list categories. Gear is
// faction-agnostic; the category selects the Mongo collection.
var GearCategories = map[string]bool{
	"weapons": true, "stratagems": true, "armor": true, "boosters": true,
}

// UnitCollections holds enemy units plus same-faction structures (bug holes,
// fabricators, warp ships): both answer "how do I kill X".
var UnitCollections = []string{"units", "structures"}

// GearCollections pools the four loadout collections for exact gear lookup.
var GearCollections = []string{"weapons", "stratagems", "armor", "boosters"}

// PlaceCollections holds static world intel (distinct from live
// planet_status): biomes, planets, missions, difficulties and effects.
var PlaceCollections = []string{"biomes", "planets", "missions", "difficulty", "effects"}

// Retrieval budgets: exact tools still cap the Mongo scan per collection.
// Collections are small (~900 docs total), so these caps bound prompt cost,
// not correctness — the match itself is exact, not ranked.
const (
	fetchCap         = 300 // per-collection scan cap for exact/list lookups
	defaultListLimit = 10  // roster default when the model omits limit
	maxListLimit     = 20  // roster ceiling: one world question's worth
)

// flat maps newlines to spaces so one doc always renders as one cited line:
// a multiline wiki-crawled field must never forge a second line in context.
func flat(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

// strList reads a doc field that may be stored as a BSON array, a Go slice,
// a generic decoded slice, or a lone string, always returning a slice. Docs
// come from two writers (seed ingest and wiki crawl) with slightly different
// shapes, so callers must not assume one representation.
func strList(m bson.M, key string) []string {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, flat(s))
		}
	}
	switch v := m[key].(type) {
	case bson.A:
		for _, e := range v {
			if s := toString(e); s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, e := range v {
			if s := toString(e); s != "" {
				out = append(out, s)
			}
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	case string:
		add(v)
	}
	return out
}

// docName returns a doc's display name, falling back to "?" so a malformed
// doc still renders a citable line instead of an empty bullet.
func docName(m bson.M) string {
	if s, ok := m["name"].(string); ok && s != "" {
		return flat(s)
	}
	return "?"
}

// normalize folds a lookup name for exact comparison: trimmed lowercase.
// Alias resolution lives here, server-side, so the model never has to guess
// canonical names — "hulk", "Hulk Bruiser" and " HULK " all hit one doc.
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// isExactMatch reports whether doc's canonical name or any alias equals
// want (case-insensitive, whitespace-trimmed). Empty want never matches.
func isExactMatch(doc bson.M, want string) bool {
	want = normalize(want)
	if want == "" {
		return false
	}
	if normalize(docName(doc)) == want {
		return true
	}
	for _, a := range strList(doc, "aliases") {
		if normalize(a) == want {
			return true
		}
	}
	return false
}

// FindExact returns the first doc in docs whose name or alias exactly
// matches name (case-insensitive). Pure so tests pin alias resolution
// without a database.
func FindExact(docs []bson.M, name string) (bson.M, bool) {
	for _, d := range docs {
		if isExactMatch(d, name) {
			return d, true
		}
	}
	return nil, false
}

// FilterUnits narrows docs to one faction with an optional role substring
// (case-insensitive, matched against role or note so structures with a note
// but no role still filter). Faction must already be validated against
// Factions; unknown factions yield no docs rather than a wrong roster. Pure
// for tests.
func FilterUnits(docs []bson.M, faction, role string) []bson.M {
	faction = normalize(faction)
	role = normalize(role)
	var out []bson.M
	for _, d := range docs {
		if normalize(toString(d["faction"])) != faction {
			continue
		}
		if role != "" {
			if !strings.Contains(strings.ToLower(toString(d["role"])), role) &&
				!strings.Contains(strings.ToLower(toString(d["note"])), role) {
				continue
			}
		}
		out = append(out, d)
	}
	return out
}

// findAll loads up to limit docs from a collection, excluding the embedding
// vector and the legacy text_blob (payload weight with no retrieval value
// now that ranking is gone). Any failure yields nil — the caller degrades
// to fewer docs rather than an error reply — with a debug log so silent
// Mongo outages stay visible in ops.
func findAll(ctx context.Context, db *mongo.Database, coll string, filter bson.M, limit int64) []bson.M {
	cur, err := db.Collection(coll).Find(ctx, filter,
		options.Find().SetLimit(limit).SetProjection(bson.M{"embedding": 0, "text_blob": 0}))
	if err != nil {
		log.Printf("mcpdb: find %s: %v", coll, err)
		return nil
	}
	defer cur.Close(ctx)
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		log.Printf("mcpdb: decode %s: %v", coll, err)
		return nil
	}
	// Visible growth signal: an exact name past the cap silently misses, so
	// a full page must surface in ops before it becomes a miss.
	if int64(len(docs)) == limit {
		log.Printf("mcpdb: %s hit fetch cap %d", coll, limit)
	}
	return docs
}

// clampLimit bounds a roster limit, defaulting non-positive values to
// defaultListLimit and capping at maxListLimit.
func clampLimit(n int) int {
	if n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// clampOffset floors a roster offset at zero: negative offsets read from
// the start instead of panicking the slice.
func clampOffset(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// paginate cuts a sorted roster to [offset, offset+limit). Pure so tests pin
// the list-then-fetch discoverability contract without a database.
func paginate(docs []bson.M, limit, offset int) []bson.M {
	limit = clampLimit(limit)
	offset = clampOffset(offset)
	if offset >= len(docs) {
		return nil
	}
	docs = docs[offset:]
	if len(docs) > limit {
		docs = docs[:limit]
	}
	return docs
}

// byName orders docs by normalized name so the roster is deterministic and
// case-insensitive across identical queries.
func byName(docs []bson.M) {
	sort.SliceStable(docs, func(i, j int) bool { return normalize(docName(docs[i])) < normalize(docName(docs[j])) })
}

// GetUnit fetches one enemy unit or structure by exact name or alias
// (case-insensitive), searching units then structures. Returns the doc, the
// collection that held it, and whether it was found.
//
// Small-scale scan by design: at ~900 docs a bounded per-collection fetch
// plus Go exact-match is deterministic and keeps alias resolution in one
// place. The unique (faction, name) indexes from ingest enforce identity;
// they are not the lookup path.
func GetUnit(ctx context.Context, db *mongo.Database, name string) (bson.M, string, bool) {
	if normalize(name) == "" {
		return nil, "", false
	}
	for _, coll := range UnitCollections {
		docs := findAll(ctx, db, coll, bson.M{}, fetchCap)
		// Deterministic tie-break: each shelf scans in name order and
		// shelves in UnitCollections order, so units win ties over
		// structures. Deterministic is the contract; units-first is the
		// documented precedence.
		byName(docs)
		for _, d := range docs {
			if isExactMatch(d, name) {
				return d, coll, true
			}
		}
	}
	return nil, "", false
}

// ListUnits returns the faction-filtered roster (units plus structures),
// optionally narrowed by a role substring, paged by limit/offset. Sorted by
// normalized name so every page is deterministic; offset keeps names beyond
// the first page discoverable via list-then-fetch.
func ListUnits(ctx context.Context, db *mongo.Database, faction, role string, limit, offset int) []bson.M {
	faction = normalize(faction)
	if !Factions[faction] {
		return nil
	}
	var docs []bson.M
	for _, coll := range UnitCollections {
		docs = append(docs, findAll(ctx, db, coll, bson.M{}, fetchCap)...)
	}
	out := FilterUnits(docs, faction, role)
	byName(out)
	return paginate(out, limit, offset)
}

// GetGear fetches one gear doc by exact name or alias across the four gear
// collections. Returns the doc, the collection (category) that held it, and
// whether it was found. Same small-scale scan contract as GetUnit: bounded
// fetches plus Go exact-match; the unique name indexes enforce identity.
func GetGear(ctx context.Context, db *mongo.Database, name string) (bson.M, string, bool) {
	if normalize(name) == "" {
		return nil, "", false
	}
	for _, coll := range GearCollections {
		docs := findAll(ctx, db, coll, bson.M{}, fetchCap)
		// Same deterministic tie-break as GetUnit: name order within the
		// shelf, shelves in GearCollections order.
		byName(docs)
		for _, d := range docs {
			if isExactMatch(d, name) {
				return d, coll, true
			}
		}
	}
	return nil, "", false
}

// ListGear returns the roster of one gear category, paged by limit/offset
// and sorted by normalized name. Unknown categories yield no docs rather
// than a wrong shelf.
func ListGear(ctx context.Context, db *mongo.Database, category string, limit, offset int) []bson.M {
	category = normalize(category)
	if !GearCategories[category] {
		return nil
	}
	docs := findAll(ctx, db, category, bson.M{}, fetchCap)
	byName(docs)
	return paginate(docs, limit, offset)
}

// GetPlace fetches one static world doc (biome, planet, mission,
// difficulty, effect) by exact name or alias. Live ownership stays on
// planet_status — this is the static intel shelf. Returns the doc, the
// collection that held it, and whether it was found. Same small-scale scan
// contract as GetUnit; the unique name indexes enforce identity.
func GetPlace(ctx context.Context, db *mongo.Database, name string) (bson.M, string, bool) {
	if normalize(name) == "" {
		return nil, "", false
	}
	for _, coll := range PlaceCollections {
		docs := findAll(ctx, db, coll, bson.M{}, fetchCap)
		// Same deterministic tie-break as GetUnit: name order within the
		// shelf, shelves in PlaceCollections order.
		byName(docs)
		for _, d := range docs {
			if isExactMatch(d, name) {
				return d, coll, true
			}
		}
	}
	return nil, "", false
}

// cut truncates to n runes (multibyte-safe) and flattens newlines, so rich
// excerpts never split a rune the model then echoes back as replacement
// characters, nor forge a second cited line.
func cut(s string, n int) string {
	s = flat(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// docSource renders a doc's provenance for citations: source_url wins, then
// source, then the mongo fallback so every line stays citable.
func docSource(d bson.M) string {
	if src, _ := d["source_url"].(string); src != "" {
		return flat(src)
	}
	if s, ok := d["source"].(string); ok && s != "" {
		return flat(s)
	}
	return "mongo"
}

// FormatContext renders retrieved docs as cited context lines for the
// system prompt. Every line carries weak point + counter + source, which is
// what lets the model answer with citations instead of bare assertions.
// Field fallbacks (effect_text, procurement) exist because gear and booster
// docs use different schemas than unit docs. Lists ride on this short shape;
// single exact fetches use FormatFull for the richer excerpt.
func FormatContext(docs []bson.M) string {
	var lines []string
	for _, d := range docs {
		src := docSource(d)
		weak := strings.Join(strList(d, "weak_points"), " ")
		if weak == "" {
			weak = toString(d["effect_text"])
		}
		counters := strings.Join(strList(d, "counters"), " ")
		if counters == "" {
			counters = toString(d["procurement"])
		}
		lines = append(lines, "- "+docName(d)+": weak "+weak+" | counter "+counters+" [source: "+src+"]")
	}
	return strings.Join(lines, "\n")
}

// FormatFull renders one exact-match doc as a rich cited excerpt: role or
// description, weak points, counters, plus the extra detail the short roster
// lines drop (health/size/difficulty, anatomy, tactics, conditions, gear
// params). Single line, rune-capped, so it fits the tool-output budget.
func FormatFull(d bson.M) string {
	src := docSource(d)
	role := toString(d["role"])
	if role == "" {
		if desc := toString(d["description"]); desc != "" {
			role = desc
		} else if note := toString(d["note"]); note != "" {
			role = note
		} else if wt := toString(d["wiki_text"]); wt != "" {
			role = cut(wt, 200)
		}
	}
	weak := strings.Join(strList(d, "weak_points"), "; ")
	if weak == "" {
		weak = toString(d["effect_text"])
	}
	counters := strings.Join(strList(d, "counters"), "; ")
	if counters == "" {
		counters = toString(d["procurement"])
	}
	var extra []string
	if h := healthOf(d); h != "" {
		extra = append(extra, "health "+h)
	}
	if s := toString(d["size"]); s != "" {
		extra = append(extra, "size "+s)
	}
	if md := toString(d["min_difficulty"]); md != "" {
		extra = append(extra, "difficulty "+md)
	}
	if a := anatomySummary(d); a != "" {
		extra = append(extra, "anatomy "+a)
	}
	if t := tacticsSummary(d); t != "" {
		extra = append(extra, "tactics "+t)
	}
	if c := conditionsSummary(d); c != "" {
		extra = append(extra, c)
	}
	if p := paramsSummary(d); p != "" {
		extra = append(extra, p)
	}
	line := "- " + docName(d)
	if f := toString(d["faction"]); f != "" {
		line += " [" + f + "]"
	}
	if role != "" {
		line += ": " + cut(role, 300)
	}
	if weak != "" {
		line += " | weak " + cut(weak, 300)
	}
	if counters != "" {
		line += " | counter " + cut(counters, 300)
	}
	if len(extra) > 0 {
		line += " | " + cut(strings.Join(extra, "; "), 500)
	}
	return line + " [source: " + src + "]"
}

// toString coerces a doc value to single-line text: strings pass through,
// numerics and bools render via fmt.Sprint, nil and other shapes read empty.
// Keeps rich excerpts from silently dropping wiki-crawl numerics or forging
// extra lines from embedded newlines.
func toString(v any) string {
	if s, ok := v.(string); ok {
		return flat(s)
	}
	switch v.(type) {
	case nil:
		return ""
	case bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		json.Number:
		return flat(fmt.Sprint(v))
	default:
		return ""
	}
}

// healthOf reads health stored as BSON numerics or a comma-grouped string.
// Anything non-numeric (or zero) reads empty; bools never render as health.
func healthOf(d bson.M) string {
	if _, ok := d["health"].(bool); ok {
		return ""
	}
	switch h := toString(d["health"]); h {
	case "", "0":
		return ""
	default:
		return h
	}
}

// anatomyRow reads one anatomy row stored as BSON or plain maps,
// coercing non-string AV/fatal numerics instead of dropping them.
func anatomyRow(m map[string]any) (part, av, fatal string) {
	part = toString(m["part"])
	av = toString(m["av"])
	fatal = toString(m["fatal"])
	return part, av, fatal
}

// anatomySummary condenses anatomy rows to "part (AV fatal)" triples,
// accepting BSON and plain decoded shapes.
func anatomySummary(d bson.M) string {
	var rows []any
	switch v := d["anatomy"].(type) {
	case bson.A:
		rows = []any(v)
	case []any:
		rows = v
	default:
		return ""
	}
	var out []string
	for _, e := range rows {
		var m map[string]any
		switch t := e.(type) {
		case bson.M:
			m = map[string]any(t)
		case map[string]any:
			m = t
		default:
			continue
		}
		part, av, fatal := anatomyRow(m)
		if part == "" {
			continue
		}
		detail := part
		if av != "" {
			detail += " AV:" + av
		}
		if fatal != "" {
			detail += " fatal:" + fatal
		}
		out = append(out, detail)
		if len(out) >= 4 {
			break
		}
	}
	return strings.Join(out, ", ")
}

// tacticsSummary joins the first two wiki tactics lines.
func tacticsSummary(d bson.M) string {
	t := strList(d, "wiki_tactics")
	if len(t) > 2 {
		t = t[:2]
	}
	return strings.Join(t, "; ")
}

// conditionsSummary joins biome/planet condition fields.
func conditionsSummary(d bson.M) string {
	var parts []string
	for _, k := range []string{"conditions", "hazards", "weather"} {
		if items := strList(d, k); len(items) > 0 {
			if len(items) > 3 {
				items = items[:3]
			}
			parts = append(parts, k+" "+strings.Join(items, ", "))
		}
	}
	if a := toString(d["archetype"]); a != "" {
		parts = append(parts, "archetype "+a)
	}
	return strings.Join(parts, "; ")
}

// paramsSummary renders generic gear/world infobox params as k=v pairs,
// accepting BSON and plain decoded shapes and coercing non-string values
// instead of dropping them.
func paramsSummary(d bson.M) string {
	var m map[string]any
	switch v := d["params"].(type) {
	case bson.M:
		m = map[string]any(v)
	case map[string]any:
		m = v
	case bson.D:
		m = map[string]any{}
		for _, e := range v {
			m[e.Key] = e.Value
		}
	default:
		return ""
	}
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		if s := toString(m[k]); s != "" {
			pairs = append(pairs, flat(k)+"="+cut(s, 80))
			if len(pairs) >= 6 {
				break
			}
		}
	}
	return strings.Join(pairs, ", ")
}
