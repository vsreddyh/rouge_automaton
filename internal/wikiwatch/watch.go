// Package wikiwatch keeps cached wiki docs fresh: poll the RecentChanges
// Atom feed, diff revision ids against tracked_pages, and re-parse only the
// changed pages. Ports scripts/rss_watch.py plus the refresh path of
// scripts/crawl_wiki.py, with no Python dependency.
package wikiwatch

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/vsreddyh/rouge_automaton/internal/wikifeed"
	"github.com/vsreddyh/rouge_automaton/internal/wikiparse"
)

// Tracked is a row of tracked_pages.
type Tracked struct {
	Title      string
	Collection string
	RevID      int
}

// Change is a detected page edit that needs a refresh.
type Change struct {
	Tracked   Tracked
	Canonical string
	NewRev    int
}

// Watcher diffs the wiki feed against Mongo and refreshes changed docs.
type Watcher struct {
	DB     *mongo.Database
	Feed   *wikifeed.Client
	Days   int
	Limit  int
	lastLM string
}

// New builds a watcher with sensible feed defaults.
func New(db *mongo.Database, feed *wikifeed.Client) *Watcher {
	if feed == nil {
		feed = wikifeed.New()
	}
	return &Watcher{DB: db, Feed: feed, Days: 1, Limit: 50}
}

// Tracked returns the tracked_pages row for a title, if any.
func (w *Watcher) Tracked(ctx context.Context, title string) (Tracked, bool) {
	var doc bson.M
	err := w.DB.Collection("tracked_pages").FindOne(ctx, bson.M{"title": title}).Decode(&doc)
	if err != nil {
		return Tracked{}, false
	}
	return trackedFrom(doc), true
}

func trackedFrom(doc bson.M) Tracked {
	t := Tracked{}
	if s, ok := doc["title"].(string); ok {
		t.Title = s
	}
	if s, ok := doc["collection"].(string); ok {
		t.Collection = s
	}
	t.RevID = numInt(doc["rev_id"])
	return t
}

// numInt coerces a BSON numeric (int32, int64, float64) to int.
func numInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return 0
}

// Poll fetches the feed and returns tracked pages whose revision changed.
// A 304 Not Modified yields no changes.
func (w *Watcher) Poll(ctx context.Context) ([]Change, error) {
	entries, lm, err := w.Feed.Feed(ctx, w.Days, w.Limit, w.lastLM)
	if err != nil {
		return nil, err
	}
	if lm != "" {
		w.lastLM = lm
	}
	var changes []Change
	for _, e := range entries {
		title := strings.TrimSpace(e.Title)
		if title == "" || strings.Contains(title, ":") {
			continue
		}
		tr, ok := w.Tracked(ctx, title)
		if !ok {
			continue
		}
		canon, rev, err := w.Feed.Revision(ctx, title)
		if err != nil || rev <= 0 {
			continue
		}
		if canon != tr.Title {
			if alt, ok := w.Tracked(ctx, canon); ok {
				tr = alt
			}
		}
		if rev == tr.RevID {
			continue
		}
		changes = append(changes, Change{Tracked: tr, Canonical: canon, NewRev: rev})
	}
	return changes, nil
}

// Refresh re-fetches one changed page and updates its doc in place. Curated
// seed fields (weak_points, counters, aliases, role) are preserved.
func (w *Watcher) Refresh(ctx context.Context, ch Change) error {
	page, err := w.Feed.Page(ctx, ch.Canonical)
	if err != nil {
		return err
	}
	if page.RevID == 0 {
		return nil
	}
	coll := ch.Tracked.Collection
	if coll == "" {
		return nil
	}
	title := page.Title
	if title == "" {
		title = ch.Canonical
	}
	sourceURL := w.FeedURLBase() + "/wiki/" + wikifeed.Slug(title)
	filter := bson.M{"source_url": sourceURL}
	var cur bson.M
	err = w.DB.Collection(coll).FindOne(ctx, filter).Decode(&cur)
	if err != nil {
		// Fall back to name match for docs seeded before the first crawl.
		filter = bson.M{"name": title}
		cur = nil
		if err := w.DB.Collection(coll).FindOne(ctx, filter).Decode(&cur); err != nil {
			log.Printf("watch: no doc for %s (%s)", title, coll)
			return nil
		}
	}
	set := w.buildFields(coll, page, cur, sourceURL)
	if _, err := w.DB.Collection(coll).UpdateOne(ctx, filter, bson.M{"$set": set}); err != nil {
		return err
	}
	_, err = w.DB.Collection("tracked_pages").UpdateOne(ctx,
		bson.M{"title": ch.Tracked.Title},
		bson.M{"$set": bson.M{
			"url": sourceURL, "collection": coll, "rev_id": page.RevID,
		}})
	return err
}

// FeedURLBase exposes the wiki base URL for building source links.
func (w *Watcher) FeedURLBase() string {
	if w.Feed != nil && w.Feed.Base != "" {
		return strings.TrimSuffix(w.Feed.Base, "/")
	}
	return wikifeed.DefaultBase
}

func (w *Watcher) buildFields(coll string, page wikifeed.Page, cur bson.M, sourceURL string) bson.M {
	set := bson.M{
		"source_url": sourceURL,
		"rev_id":     page.RevID,
		"fetched_at": page.Timestamp,
	}
	switch coll {
	case "units", "structures":
		box := wikiparse.Infobox(page.Wikitext)
		anatomy := wikiparse.Anatomy(page.Wikitext, 12)
		tactics := wikiparse.Tactics(page.Wikitext, 8)
		diff := wikiparse.DifficultyName(box["min_difficulty"])
		if diff == "" {
			diff = wikiparse.SpawningDifficulty(page.Wikitext)
		}
		rows := make(bson.A, 0, len(anatomy))
		for _, r := range anatomy {
			rows = append(rows, bson.M{
				"part": r.Part, "health": r.Health, "av": r.AV, "fatal": r.Fatal,
			})
		}
		set["health"] = wikiparse.Health(box["health"])
		set["size"] = wikiparse.SizeName(box["size"])
		set["min_difficulty"] = diff
		set["damage_types"] = wikiparse.DamageTypes(box["damage_type"])
		set["fire_mult"] = box["fire_mult"]
		set["anatomy"] = rows
		set["wiki_tactics"] = tactics
		set["wiki_text"] = cut(page.Extract, 4000)
		set["text_blob"] = unitBlob(cur, page.Extract, tactics)
	case "biomes":
		box := wikiparse.Infobox(page.Wikitext)
		conds, ok := wikiparse.SectionList(page.Wikitext, "Environmental Conditions", 12)
		haz, _ := wikiparse.SectionList(page.Wikitext, "Environmental Hazards", 12)
		wx, _ := wikiparse.SectionList(page.Wikitext, "Weather", 12)
		note := ""
		if len(conds) == 0 && ok {
			note = "No environmental conditions specific to this biome."
		}
		set["archetype"] = box["archetype"]
		set["internal_name"] = box["internal_name"]
		set["description"] = cut(box["description"], 500)
		set["conditions"] = toBsonA(conds)
		set["hazards"] = toBsonA(haz)
		set["weather"] = toBsonA(wx)
		set["note"] = note
		set["text_blob"] = strings.Join([]string{nameOf(cur), cut(page.Extract, 2000)}, " | ")
	default:
		box := cleanParams(wikiparse.Infobox(page.Wikitext))
		params := bson.M{}
		keys := make([]string, 0, len(box))
		for k := range box {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		vals := make([]string, 0, len(keys))
		for _, k := range keys {
			params[k] = box[k]
			vals = append(vals, box[k])
		}
		set["params"] = params
		set["text_blob"] = strings.Join([]string{nameOf(cur), strings.Join(vals, " "), cut(page.Extract, 1500)}, " | ")
	}
	return set
}

// cleanParams mirrors the Python crawler: drop image/caption noise and
// over-long values so keyword scoring is not polluted.
func cleanParams(box map[string]string) map[string]string {
	drop := map[string]bool{
		"image": true, "image_caption": true, "caption_landscape": true,
		"landscape": true, "title": true,
	}
	out := map[string]string{}
	for k, v := range box {
		if drop[k] || len(v) > 250 {
			continue
		}
		out[k] = v
	}
	return out
}

// Backfill populates tracked_pages from existing docs that carry a source_url,
// so the very first poll can detect changes. Returns the number of rows seen.
func (w *Watcher) Backfill(ctx context.Context) (int, error) {
	collections := []string{"units", "biomes", "structures", "boosters",
		"missions", "weapons", "stratagems", "armor"}
	seen := 0
	for _, coll := range collections {
		cur, err := w.DB.Collection(coll).Find(ctx, bson.M{},
			options.Find().SetProjection(bson.M{"source_url": 1, "rev_id": 1, "name": 1}))
		if err != nil {
			continue
		}
		for cur.Next(ctx) {
			var doc bson.M
			if err := cur.Decode(&doc); err != nil {
				continue
			}
			url, _ := doc["source_url"].(string)
			if !strings.Contains(url, "/wiki/") {
				continue
			}
			title := strings.ReplaceAll(url[strings.LastIndex(url, "/wiki/")+len("/wiki/"):], "_", " ")
			rev := numInt(doc["rev_id"])
			res, err := w.DB.Collection("tracked_pages").UpdateOne(ctx,
				bson.M{"title": title},
				bson.M{"$set": bson.M{"url": url, "collection": coll, "rev_id": rev}},
				options.UpdateOne().SetUpsert(true))
			// Count only rows this run created: re-fetched rows report
			// MatchedCount, so restarts no longer overstate the log line.
			if err == nil && res.UpsertedCount > 0 {
				seen++
			}
		}
		if err := cur.Close(ctx); err != nil {
			log.Printf("watch: cursor close: %v", err)
		}
	}
	return seen, nil
}

// RunOnce polls and refreshes every changed page. Returns the change count.
func (w *Watcher) RunOnce(ctx context.Context) (int, error) {
	changes, err := w.Poll(ctx)
	if err != nil {
		return 0, err
	}
	for _, ch := range changes {
		log.Printf("watch: %s (%s) rev %d -> %d", ch.Tracked.Title, ch.Tracked.Collection, ch.Tracked.RevID, ch.NewRev)
		if err := w.Refresh(ctx, ch); err != nil {
			log.Printf("watch: refresh %s: %v", ch.Tracked.Title, err)
		}
	}
	return len(changes), nil
}

// Run polls on an interval until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	for {
		if n, err := w.RunOnce(ctx); err != nil {
			log.Printf("watch: poll: %v", err)
		} else if n > 0 {
			log.Printf("watch: refreshed %d page(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func nameOf(doc bson.M) string {
	if doc == nil {
		return ""
	}
	if s, ok := doc["name"].(string); ok {
		return s
	}
	return ""
}

func toBsonA(items []string) bson.A {
	out := make(bson.A, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// unitBlob mirrors the Python crawler's text_blob join for units.
func unitBlob(cur bson.M, extract string, tactics []string) string {
	aliases := strListOf(cur, "aliases")
	weak := strListOf(cur, "weak_points")
	counters := strListOf(cur, "counters")
	faction, _ := cur["faction"].(string)
	role, _ := cur["role"].(string)
	return strings.Join([]string{
		nameOf(cur), faction, strings.Join(aliases, " "), role,
		strings.Join(weak, " "), strings.Join(counters, " "),
		strings.Join(tactics, " "), cut(extract, 1500),
	}, " | ")
}

func strListOf(doc bson.M, key string) []string {
	if doc == nil {
		return nil
	}
	switch v := doc[key].(type) {
	case bson.A:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		return []string{v}
	}
	return nil
}
