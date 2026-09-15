// Package rag implements Type-U/G/W/L routing and Mongo retrieval.
//
// The store holds one collection per content kind (units, weapons, biomes,
// planets, ...). Retrieval never scans everything at once: the query is
// routed to the collections that can answer it, candidates are ranked in
// Go by keyword overlap with a name/alias bonus, and only the top few docs
// are injected into the prompt. There is intentionally no vector index —
// at ~900 docs the keyword scorer is cheaper, deterministic, and testable.
package rag

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var (
	// autoRe matches Automaton-side vocabulary: unit names, vehicles and
	// structures. This is a pre-filter, not a classifier — a matching query
	// simply earns a Type-U lookup alongside any other routed types.
	autoRe = regexp.MustCompile(`(?i)hulk|tank|devastator|strider|bot|dropship|gunship|fabricator|raider|berserker|commissar|trooper`)
	// termRe matches Terminid-side vocabulary (units and bug holes).
	termRe = regexp.MustCompile(`(?i)charger|titan|bug|terminid|spewer|spitter|stalker|impaler|hunter|warrior|hole|bile`)
	// illuRe matches Illuminate-side vocabulary.
	illuRe = regexp.MustCompile(`(?i)squid|illuminate|harvester|overseer|voteless|watcher|warp|stingray|fleshmob`)
	// gearRe matches loadout vocabulary: stratagems, weapon classes and
	// attachment-adjacent terms. Triggers a Type-G lookup.
	gearRe = regexp.MustCompile(`(?i)stratagem|weapon|loadout|amr|railgun|eagle|orbital|sentry|armor|booster|warbond|primary|secondary|throwable|rifle|shotgun|pistol|smg|support weapon|grenade`)
	// worldRe matches planet/biome/mission vocabulary, including concrete
	// biome names so "tundra conditions" routes to Type-W without the word
	// "biome" being present.
	worldRe = regexp.MustCompile(`(?i)planet|biome|mission|difficulty|storm|fog|blizzard|where|liberat|sector|weather|hazard|tundra|desert|swamp|forest|jungle|arctic|oasis|moor|metropolis|colony|conditions|glacier|dunes`)
	// liveRe matches war-status vocabulary. The word-boundary on owns avoids
	// false positives like "known". Triggers a Type-L live API lookup.
	liveRe = regexp.MustCompile(`(?i)status|order|owner|\bowns?\b|controlled|liberation|players|divers|attack|defense|campaign|who holds|right now`)
	// wordRe tokenizes queries for keyword extraction.
	wordRe = regexp.MustCompile(`\w+`)
)

// Stop is the noise-word set stripped before scoring — the same list as the
// Python port. Without it, queries like "how to kill hulk" let "how/to"
// dominate the ranking.
var Stop = map[string]bool{
	"how": true, "what": true, "why": true, "when": true, "where": true,
	"which": true, "with": true, "from": true, "that": true, "this": true,
	"they": true, "them": true, "then": true, "than": true, "your": true,
	"about": true, "into": true, "does": true, "the": true, "and": true,
	"for": true, "are": true, "you": true, "me": true, "my": true,
	"on": true, "in": true, "of": true, "a": true, "an": true,
	"is": true, "it": true, "to": true, "be": true, "by": true,
	"or": true, "do": true, "i": true, "as": true, "at": true,
}

// Route maps a query to the retrieval types that can answer it: U (units),
// G (gear), W (world), L (live). A query may match several ("loadout for
// Mintoria difficulty 6" is G+W); a query matching nothing falls back to U,
// the most likely intent for this bot.
func Route(query string) map[string]bool {
	types := map[string]bool{}
	if autoRe.MatchString(query) || termRe.MatchString(query) || illuRe.MatchString(query) {
		types["U"] = true
	}
	if gearRe.MatchString(query) {
		types["G"] = true
	}
	if worldRe.MatchString(query) {
		types["W"] = true
	}
	if liveRe.MatchString(query) {
		types["L"] = true
	}
	if len(types) == 0 {
		types["U"] = true
	}
	return types
}

// FactionOf narrows a Type-U lookup to one faction collection slice, so a
// Hulk question never sees Terminid docs. Illuminate is checked first
// because its vocabulary is the most distinctive; "" means no filter.
func FactionOf(query string) string {
	switch {
	case illuRe.MatchString(query):
		return "illuminate"
	case termRe.MatchString(query):
		return "terminids"
	case autoRe.MatchString(query):
		return "automatons"
	}
	return ""
}

// Keywords extracts the significant lowercase terms of a query, each once.
// Terms of length <=2 and stopwords are dropped; deduplication matters
// because Score counts occurrences ("hulk hulk hulk" must not triple-count).
func Keywords(query string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(query), -1) {
		if len(w) > 2 && !Stop[w] && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

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

// Score ranks one candidate doc against the query: the sum of keyword
// occurrence counts in its search blob, plus a +10 bonus when the query
// names the doc exactly or uses one of its aliases. The bonus is what keeps
// "Hulk Bruiser" above keyword-rich but unrelated docs; the comma-ok read
// on text_blob keeps docs that lack the field from panicking.
func Score(query string, words []string, d bson.M) float64 {
	name := docName(d)
	tb, _ := d["text_blob"].(string)
	blob := strings.ToLower(tb + " " + name)
	var s float64
	for _, w := range words {
		s += float64(strings.Count(blob, w))
	}
	ql := strings.ToLower(query)
	if name != "" && (strings.Contains(ql, strings.ToLower(name))) {
		s += 10
	} else {
		for _, a := range strList(d, "aliases") {
			if a != "" && strings.Contains(ql, strings.ToLower(a)) {
				s += 10
				break
			}
		}
	}
	return s
}

// rank orders candidates best-first in place. SliceStable keeps Mongo's
// return order for ties, so results are deterministic across identical
// queries.
func rank(query string, words []string, docs []bson.M) {
	sort.SliceStable(docs, func(i, j int) bool {
		return Score(query, words, docs[i]) > Score(query, words, docs[j])
	})
}

// findAll loads up to limit docs from a collection, excluding any embedding
// vector (payload weight with no retrieval value at query time). Any
// failure yields nil — the caller ranks whatever it got, so a down
// collection degrades to fewer context docs rather than an error reply.
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

// SearchUnits serves Type-U: the faction slice of units plus same-faction
// structures (bug holes, fabricators), ranked together and cut to k.
// Structures share the query vocabulary, so closing a bug hole ranks
// alongside killing the bugs that use it.
func SearchUnits(ctx context.Context, db *mongo.Database, query string, k int) []bson.M {
	filter := bson.M{}
	if f := FactionOf(query); f != "" {
		filter["faction"] = f
	}
	docs := findAll(ctx, db, "units", filter, 100)
	docs = append(docs, findAll(ctx, db, "structures", filter, 40)...)
	rank(query, Keywords(query), docs)
	if len(docs) > k {
		docs = docs[:k]
	}
	return docs
}

// SearchGear serves Type-G: stratagems, weapons, armor and boosters pooled
// and ranked together, cut to k. No faction filter applies — gear is
// faction-agnostic, and the counters inside unit docs already name the
// right tools.
func SearchGear(ctx context.Context, db *mongo.Database, query string, k int) []bson.M {
	var docs []bson.M
	for _, coll := range []string{"stratagems", "weapons", "armor", "boosters"} {
		docs = append(docs, findAll(ctx, db, coll, bson.M{}, 150)...)
	}
	rank(query, Keywords(query), docs)
	if len(docs) > k {
		docs = docs[:k]
	}
	return docs
}

// SearchWorld serves Type-W in three passes: ranked biomes (the whole set
// is only ~31 docs), planets matched exactly on name/sector keywords, then
// missions/difficulty/effects ranked and capped at 2 — the Python parity
// cap that keeps world answers from drowning in modifier docs.
func SearchWorld(ctx context.Context, db *mongo.Database, query string, k int) []bson.M {
	words := Keywords(query)
	biomes := findAll(ctx, db, "biomes", bson.M{}, 31)
	rank(query, words, biomes)
	if len(biomes) > k {
		biomes = biomes[:k]
	}
	out := append([]bson.M{}, biomes...)
	// seen rebuilds the word set for planet matching: biomes above consumed
	// the ranked slice, but planet matching is exact-substring, not scored.
	seen := map[string]bool{}
	for _, w := range words {
		seen[w] = true
	}
	for _, p := range findAll(ctx, db, "planets", bson.M{}, 280) {
		name, _ := p["name"].(string)
		sector, _ := p["sector"].(string)
		blob := strings.ToLower(name + sector)
		for w := range seen {
			if strings.Contains(blob, w) {
				out = append(out, p)
				break
			}
		}
		if len(out) >= k+2 {
			break
		}
	}
	var extra []bson.M
	for _, coll := range []string{"missions", "difficulty", "effects"} {
		extra = append(extra, findAll(ctx, db, coll, bson.M{}, 120)...)
	}
	rank(query, words, extra)
	if len(extra) > 2 {
		extra = extra[:2]
	}
	out = append(out, extra...)
	if len(out) > k+4 {
		out = out[:k+4]
	}
	return out
}

// Retrieve fans out across every routed type (3 unit + 2 gear + 2 world
// docs typical) and caps the merged set at 5, holding the prompt budget
// near ~2500 input tokens regardless of how many types matched.
func Retrieve(ctx context.Context, db *mongo.Database, query string) ([]bson.M, map[string]bool) {
	// TODO: L-only queries return no docs (same gap as the Python port); the
	// caller serves those from the live status API instead.
	types := Route(query)
	var docs []bson.M
	if types["U"] {
		docs = append(docs, SearchUnits(ctx, db, query, 3)...)
	}
	if types["G"] {
		docs = append(docs, SearchGear(ctx, db, query, 2)...)
	}
	if types["W"] {
		docs = append(docs, SearchWorld(ctx, db, query, 2)...)
	}
	if len(docs) > 5 {
		docs = docs[:5]
	}
	return docs, types
}

// FormatContext renders retrieved docs as cited context lines for the
// system prompt. Every line carries weak point + counter + source, which is
// what lets the model answer with citations instead of bare assertions.
// Field fallbacks (effect_text, procurement) exist because gear and booster
// docs use different schemas than unit docs.
func FormatContext(docs []bson.M) string {
	var lines []string
	for _, d := range docs {
		src, _ := d["source_url"].(string)
		if src == "" {
			if s, ok := d["source"].(string); ok {
				src = s
			} else {
				src = "mongo"
			}
		}
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
