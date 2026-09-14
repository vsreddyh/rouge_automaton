// Package rag implements Type-U/G/W/L routing and Mongo retrieval.
// Ports bot/rag.py: keyword-first rank with name/alias boost, no vectors.
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
	autoRe  = regexp.MustCompile(`(?i)hulk|tank|devastator|strider|bot|dropship|gunship|fabricator|raider|berserker|commissar|trooper`)
	termRe  = regexp.MustCompile(`(?i)charger|titan|bug|terminid|spewer|spitter|stalker|impaler|hunter|warrior|hole|bile`)
	illuRe  = regexp.MustCompile(`(?i)squid|illuminate|harvester|overseer|voteless|watcher|warp|stingray|fleshmob`)
	gearRe  = regexp.MustCompile(`(?i)stratagem|weapon|loadout|amr|railgun|eagle|orbital|sentry|armor|booster|warbond|primary|secondary|throwable|rifle|shotgun|pistol|smg|support weapon|grenade`)
	worldRe = regexp.MustCompile(`(?i)planet|biome|mission|difficulty|storm|fog|blizzard|where|liberat|sector|weather|hazard|tundra|desert|swamp|forest|jungle|arctic|oasis|moor|metropolis|colony|conditions|glacier|dunes`)
	liveRe  = regexp.MustCompile(`(?i)status|order|owner|\bowns?\b|controlled|liberation|players|divers|attack|defense|campaign|who holds|right now`)
	wordRe  = regexp.MustCompile(`\w+`)
)

// Stop matches the Python STOP set.
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

// Route returns the retrieval types for a query: subset of U/G/W/L.
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

// FactionOf returns the faction filter for a query, or "".
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

// Keywords extracts significant lowercase terms, deduplicated.
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

func docName(m bson.M) string {
	if s, ok := m["name"].(string); ok && s != "" {
		return s
	}
	return "?"
}

// Score ranks a doc: keyword occurrence counts plus a name/alias hit bonus.
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

func rank(query string, words []string, docs []bson.M) {
	sort.SliceStable(docs, func(i, j int) bool {
		return Score(query, words, docs[i]) > Score(query, words, docs[j])
	})
}

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

// SearchUnits queries Type-U (units + structures), honoring the faction filter.
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

// SearchGear queries Type-G (stratagems, weapons, armor, boosters).
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

// SearchWorld queries Type-W (biomes, planets, missions, difficulty, effects).
func SearchWorld(ctx context.Context, db *mongo.Database, query string, k int) []bson.M {
	words := Keywords(query)
	biomes := findAll(ctx, db, "biomes", bson.M{}, 31)
	rank(query, words, biomes)
	if len(biomes) > k {
		biomes = biomes[:k]
	}
	out := append([]bson.M{}, biomes...)
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

// Retrieve fans out across routed types, max ~5 docs.
//
// TODO: L-only queries return no docs (same gap as the Python port); the
// caller serves those from the live status API instead.
func Retrieve(ctx context.Context, db *mongo.Database, query string) ([]bson.M, map[string]bool) {
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

// FormatContext renders docs as cited context lines.
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
