// Package rag is the Mongo retrieval layer: keyword-first ranking with a
// name/alias bonus, no vectors, and — deliberately — no router. There is no
// vocabulary list anywhere in this package: the model decides what to look
// up and calls the Search* functions with the user's own words. At ~900
// docs the keyword scorer is cheaper, deterministic, and testable.
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

// wordRe tokenizes queries for keyword extraction.
var wordRe = regexp.MustCompile(`\w+`)

// Factions is the closed set of valid faction filters. The model supplies
// these, so anything outside the set is rejected rather than guessed.
var Factions = map[string]bool{"automatons": true, "terminids": true, "illuminate": true}

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

// SearchUnits searches units plus same-faction structures (bug holes,
// fabricators), ranked together and cut to k. Faction is supplied by the
// caller (the model), validated against Factions; anything else means no
// filter rather than a wrong one. Structures share the query vocabulary,
// so closing a bug hole ranks alongside killing the bugs that use it.
func SearchUnits(ctx context.Context, db *mongo.Database, query, faction string, k int) []bson.M {
	filter := bson.M{}
	if Factions[strings.ToLower(faction)] {
		filter["faction"] = strings.ToLower(faction)
	}
	docs := findAll(ctx, db, "units", filter, 100)
	docs = append(docs, findAll(ctx, db, "structures", filter, 40)...)
	rank(query, Keywords(query), docs)
	if len(docs) > k {
		docs = docs[:k]
	}
	return docs
}

// SearchGear searches stratagems, weapons, armor and boosters pooled and
// ranked together, cut to k. No faction filter applies — gear is
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

// SearchWorld searches biomes, planets, missions, difficulty and effects
// in three passes: ranked biomes (the whole set is only ~31 docs), planets
// matched exactly on name/sector keywords, then missions/difficulty/effects
// ranked and capped at 2 so world answers don't drown in modifier docs.
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
