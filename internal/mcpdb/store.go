// Package mcpdb exposes the intel store as model tools. It is the single
// implementation behind two fronts: the Discord bot's think-act loop (which
// calls Execute directly) and the standalone MCP server (which serves the
// same tools over stdio). One mapping, no duplication, no router.
package mcpdb

import (
	"context"
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

// strList reads a doc field that may be stored as a BSON array, a Go slice,
// or a lone string, always returning a slice. Docs come from two writers
// (seed ingest and wiki crawl) with slightly different shapes, so callers
// must not assume one representation.
func strList(m bson.M, key string) []string {
	var out []string
	switch v := m[key].(type) {
	case bson.A:
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = v
	case string:
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// docName returns a doc's display name, falling back to "?" so a malformed
// doc still renders a citable line instead of an empty bullet.
func docName(m bson.M) string {
	if s, ok := m["name"].(string); ok && s != "" {
		return s
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
		if f, _ := d["faction"].(string); normalize(f) != faction {
			continue
		}
		if role != "" {
			r, _ := d["role"].(string)
			n, _ := d["note"].(string)
			if !strings.Contains(strings.ToLower(r), role) &&
				!strings.Contains(strings.ToLower(n), role) {
				continue
			}
		}
		out = append(out, d)
	}
	return out
}

// findAll loads up to limit docs from a collection, excluding any embedding
// vector (payload weight with no retrieval value at query time). Any
// failure yields nil — the caller degrades to fewer docs rather than an
// error reply.
func findAll(ctx context.Context, db *mongo.Database, coll string, filter bson.M, limit int64) []bson.M {
	cur, err := db.Collection(coll).Find(ctx, filter,
		options.Find().SetLimit(limit).SetProjection(bson.M{"embedding": 0}))
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil
	}
	return docs
}

// clampLimit bounds a roster limit to [1, maxListLimit], defaulting
// non-positive values to defaultListLimit.
func clampLimit(n int) int {
	if n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// GetUnit fetches one enemy unit or structure by exact name or alias
// (case-insensitive), searching units then structures. Returns the doc, the
// collection that held it, and whether it was found.
func GetUnit(ctx context.Context, db *mongo.Database, name string) (bson.M, string, bool) {
	if normalize(name) == "" {
		return nil, "", false
	}
	for _, coll := range UnitCollections {
		for _, d := range findAll(ctx, db, coll, bson.M{}, fetchCap) {
			if isExactMatch(d, name) {
				return d, coll, true
			}
		}
	}
	return nil, "", false
}

// ListUnits returns the faction-filtered roster (units plus structures),
// optionally narrowed by a role substring, cut to limit. Sorted by name so
// the roster is deterministic across identical queries.
func ListUnits(ctx context.Context, db *mongo.Database, faction, role string, limit int) []bson.M {
	limit = clampLimit(limit)
	faction = normalize(faction)
	if !Factions[faction] {
		return nil
	}
	var docs []bson.M
	for _, coll := range UnitCollections {
		docs = append(docs, findAll(ctx, db, coll, bson.M{}, fetchCap)...)
	}
	out := FilterUnits(docs, faction, role)
	sort.SliceStable(out, func(i, j int) bool { return docName(out[i]) < docName(out[j]) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// GetGear fetches one gear doc by exact name or alias across the four gear
// collections. Returns the doc, the collection (category) that held it, and
// whether it was found.
func GetGear(ctx context.Context, db *mongo.Database, name string) (bson.M, string, bool) {
	if normalize(name) == "" {
		return nil, "", false
	}
	for _, coll := range GearCollections {
		for _, d := range findAll(ctx, db, coll, bson.M{}, fetchCap) {
			if isExactMatch(d, name) {
				return d, coll, true
			}
		}
	}
	return nil, "", false
}

// ListGear returns the roster of one gear category, sorted by name and cut
// to limit. Unknown categories yield no docs rather than a wrong shelf.
func ListGear(ctx context.Context, db *mongo.Database, category string, limit int) []bson.M {
	limit = clampLimit(limit)
	category = normalize(category)
	if !GearCategories[category] {
		return nil
	}
	docs := findAll(ctx, db, category, bson.M{}, fetchCap)
	sort.SliceStable(docs, func(i, j int) bool { return docName(docs[i]) < docName(docs[j]) })
	if len(docs) > limit {
		docs = docs[:limit]
	}
	return docs
}

// GetPlace fetches one static world doc (biome, planet, mission,
// difficulty, effect) by exact name or alias. Live ownership stays on
// planet_status — this is the static intel shelf. Returns the doc, the
// collection that held it, and whether it was found.
func GetPlace(ctx context.Context, db *mongo.Database, name string) (bson.M, string, bool) {
	if normalize(name) == "" {
		return nil, "", false
	}
	for _, coll := range PlaceCollections {
		for _, d := range findAll(ctx, db, coll, bson.M{}, fetchCap) {
			if isExactMatch(d, name) {
				return d, coll, true
			}
		}
	}
	return nil, "", false
}

// cut truncates to n runes (multibyte-safe) so rich excerpts never split a
// rune the model then echoes back as replacement characters.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// docSource renders a doc's provenance for citations: source_url wins, then
// source, then the mongo fallback so every line stays citable.
func docSource(d bson.M) string {
	if src, _ := d["source_url"].(string); src != "" {
		return src
	}
	if s, ok := d["source"].(string); ok && s != "" {
		return s
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
			if e, ok := d["effect_text"].(string); ok {
				weak = e
			}
		}
		counters := strings.Join(strList(d, "counters"), " ")
		if counters == "" {
			if p, ok := d["procurement"].(string); ok {
				counters = p
			}
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
	role, _ := d["role"].(string)
	if role == "" {
		if desc, _ := d["description"].(string); desc != "" {
			role = desc
		} else if note, _ := d["note"].(string); note != "" {
			role = note
		} else if wt, _ := d["wiki_text"].(string); wt != "" {
			role = cut(wt, 200)
		}
	}
	weak := strings.Join(strList(d, "weak_points"), "; ")
	if weak == "" {
		if e, ok := d["effect_text"].(string); ok && e != "" {
			weak = e
		}
	}
	counters := strings.Join(strList(d, "counters"), "; ")
	if counters == "" {
		if p, ok := d["procurement"].(string); ok && p != "" {
			counters = p
		}
	}
	var extra []string
	if h := healthOf(d); h != "" {
		extra = append(extra, "health "+h)
	}
	if s, _ := d["size"].(string); s != "" {
		extra = append(extra, "size "+s)
	}
	if md, _ := d["min_difficulty"].(string); md != "" {
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
	if f, _ := d["faction"].(string); f != "" {
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

// healthOf reads health stored as int shapes or a comma-grouped string.
func healthOf(d bson.M) string {
	switch v := d["health"].(type) {
	case int:
		if v > 0 {
			return itoa(v)
		}
	case int32:
		if v > 0 {
			return itoa(int(v))
		}
	case int64:
		if v > 0 {
			return itoa(int(v))
		}
	case float64:
		if v > 0 {
			return itoa(int(v))
		}
	case string:
		if v != "" {
			return v
		}
	}
	return ""
}

// anatomySummary condenses anatomy rows to "part (AV fatal)" triples.
func anatomySummary(d bson.M) string {
	var rows []string
	switch v := d["anatomy"].(type) {
	case bson.A:
		for _, e := range v {
			m, ok := e.(bson.M)
			if !ok {
				continue
			}
			part, _ := m["part"].(string)
			if part == "" {
				continue
			}
			detail := part
			if av, _ := m["av"].(string); av != "" {
				detail += " AV:" + av
			}
			if fatal, _ := m["fatal"].(string); fatal != "" {
				detail += " fatal:" + fatal
			}
			rows = append(rows, detail)
			if len(rows) >= 4 {
				break
			}
		}
	}
	return strings.Join(rows, ", ")
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
	if a, _ := d["archetype"].(string); a != "" {
		parts = append(parts, "archetype "+a)
	}
	return strings.Join(parts, "; ")
}

// paramsSummary renders generic gear/world infobox params as k=v pairs.
func paramsSummary(d bson.M) string {
	m, ok := d["params"].(bson.M)
	if !ok || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			pairs = append(pairs, k+"="+cut(s, 80))
			if len(pairs) >= 6 {
				break
			}
		}
	}
	return strings.Join(pairs, ", ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
