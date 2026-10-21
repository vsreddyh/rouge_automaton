// Package wikiwatch keeps cached wiki docs fresh: poll the RecentChanges
// Atom feed, diff revision ids against tracked_pages, and re-parse only the
// changed pages. Ports scripts/rss_watch.py plus the refresh path of
// scripts/crawl_wiki.py, with no Python dependency.
package wikiwatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"rouge_automaton/internal/wikifeed"
	"rouge_automaton/internal/wikiparse"
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
//
// One watcher drives one poll loop: the queue of unseen pages and the set
// of pages that matched no shelf are kept in memory and are not safe to
// share between goroutines.
type Watcher struct {
	DB    *mongo.Database
	Feed  *wikifeed.Client
	Days  int
	Limit int
	// DiscoverLimit caps how many untracked feed entries one poll tries to
	// place on a shelf. Each attempt costs one wiki request, and the wiki
	// counts requests: a poll must not turn into a crawl.
	DiscoverLimit int
	// Backoff is how long the poller waits after the wiki says it is over
	// the rate limit (a Retry-After from the wiki wins over this).
	Backoff time.Duration
	// CrawlDelay is the pause between two page fetches in a crawl.
	CrawlDelay time.Duration
	// CrawlLimit caps how many pages one crawl fetches (0 means no cap).
	CrawlLimit int
	// untracked holds the feed entries that are not tracked yet, waiting to
	// be placed on a shelf.
	untracked []string
	// ignored holds the pages that matched no shelf. The feed keeps
	// carrying them (they are recent changes), and re-checking them would
	// spend a wiki request on every poll for nothing.
	ignored      map[string]bool
	ignoredOrder []string
	lastLM       string
}

// New builds a watcher with sensible feed defaults.
func New(db *mongo.Database, feed *wikifeed.Client) *Watcher {
	if feed == nil {
		feed = wikifeed.New()
	}
	return &Watcher{
		DB:            db,
		Feed:          feed,
		Days:          1,
		Limit:         50,
		DiscoverLimit: 5,
		Backoff:       30 * time.Minute,
		CrawlDelay:    time.Second,
		ignored:       map[string]bool{},
	}
}

// Tracked returns the tracked_pages row for a title, if any. A database
// failure is reported as an error: a blind "not tracked" would queue every
// feed entry for discovery and hammer the wiki.
func (w *Watcher) Tracked(ctx context.Context, title string) (Tracked, bool, error) {
	var doc bson.M
	err := w.DB.Collection("tracked_pages").FindOne(ctx, bson.M{"title": title}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return Tracked{}, false, nil
		}
		return Tracked{}, false, err
	}
	return trackedFrom(doc), true, nil
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
//
// The feed also carries pages the watcher has never seen: they are not
// changes yet, so they are held for the discovery pass (see Discover).
func (w *Watcher) Poll(ctx context.Context) ([]Change, error) {
	entries, lm, err := w.Feed.Feed(ctx, w.Days, w.Limit, w.lastLM)
	if err != nil {
		return nil, err
	}
	if lm != "" {
		w.lastLM = lm
	}
	var changes []Change
	tracked := map[string]Tracked{}
	var order []string
	for _, e := range entries {
		title := strings.TrimSpace(e.Title)
		if title == "" || strings.Contains(title, ":") {
			continue
		}
		tr, ok, err := w.Tracked(ctx, title)
		if err != nil {
			// The store is unreachable: do not queue the page for
			// discovery, that would spend wiki requests on a guess.
			return nil, err
		}
		if !ok {
			// An article the watcher has never seen: it may be new
			// content, and it is placed on a shelf by the discovery pass.
			w.queue(title)
			continue
		}
		if _, seen := tracked[title]; seen {
			continue
		}
		tracked[title] = tr
		order = append(order, title)
	}
	if len(order) == 0 {
		return nil, nil
	}
	// One batched revision lookup for the whole feed: the wiki counts
	// requests, and a poll must not spend one per entry.
	revs, err := w.Feed.Revisions(ctx, order)
	if err != nil {
		return nil, err
	}
	for _, title := range order {
		tr := tracked[title]
		rev, ok := revs[title]
		if !ok || rev.RevID <= 0 {
			continue
		}
		canon := rev.Title
		if canon == "" {
			canon = title
		}
		if canon != tr.Title {
			if alt, ok, err := w.Tracked(ctx, canon); err == nil && ok {
				tr = alt
			}
		}
		if rev.RevID == tr.RevID {
			continue
		}
		changes = append(changes, Change{Tracked: tr, Canonical: canon, NewRev: rev.RevID})
	}
	return changes, nil
}

// contains reports whether the slice holds the value.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// maxUntracked bounds the discovery queue. The feed carries up to Limit
// entries per poll and most of them are not shelf content; the queue must
// not grow without end, and the oldest entry is the least likely to still
// be new content.
const maxUntracked = 100

// maxIgnored bounds the set of pages that matched no shelf.
const maxIgnored = 500

// queue holds a page for the discovery pass.
func (w *Watcher) queue(title string) {
	if w.ignored[title] || contains(w.untracked, title) {
		return
	}
	if len(w.untracked) >= maxUntracked {
		w.untracked = w.untracked[1:]
	}
	w.untracked = append(w.untracked, title)
}

// ignore marks a page as belonging to no shelf, so the poller stops
// spending requests on it.
func (w *Watcher) ignore(title string) {
	if w.ignored == nil {
		w.ignored = map[string]bool{}
	}
	if w.ignored[title] {
		return
	}
	w.ignored[title] = true
	w.ignoredOrder = append(w.ignoredOrder, title)
	for len(w.ignoredOrder) > maxIgnored {
		delete(w.ignored, w.ignoredOrder[0])
		w.ignoredOrder = w.ignoredOrder[1:]
	}
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
	sourceURL := w.FeedURLBase() + "/wiki/" + wikifeed.PagePath(title)
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
	if title := titleFromURL(sourceURL); title != "" {
		set["wiki_title"] = title
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
		// The wiki infobox fills in a doc that has no curated params yet.
		// A curated params bag (the seed's numbers) is never overwritten:
		// the catalog and the wiki are both enrichment, not the seed.
		if len(curParams(cur)) == 0 {
			set["params"] = params
		} else {
			vals = curParams(cur)
		}
		set["text_blob"] = strings.Join([]string{nameOf(cur), strings.Join(vals, " "), cut(page.Extract, 1500)}, " | ")
	}
	return set
}

// curParams reads a doc's curated params bag in any writer shape.
func curParams(cur bson.M) []string {
	if cur == nil {
		return nil
	}
	var bag map[string]any
	switch t := cur["params"].(type) {
	case bson.M:
		bag = map[string]any(t)
	case map[string]any:
		bag = t
	case bson.D:
		bag = map[string]any{}
		for _, e := range t {
			bag[e.Key] = e.Value
		}
	default:
		return nil
	}
	out := make([]string, 0, len(bag))
	for k, v := range bag {
		out = append(out, fmt.Sprintf("%s=%s", k, toText(v)))
	}
	sort.Strings(out)
	return out
}

// toText renders a value as one line of text.
func toText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool:
		return fmt.Sprint(t)
	case float64:
		return fmt.Sprintf("%g", t)
	case int:
		return fmt.Sprint(t)
	case int32:
		return fmt.Sprint(t)
	case int64:
		return fmt.Sprint(t)
	}
	return fmt.Sprint(v)
}

// titleFromURL reads the page title back out of a wiki URL.
func titleFromURL(sourceURL string) string {
	if sourceURL == "" {
		return ""
	}
	idx := strings.LastIndex(sourceURL, "/wiki/")
	if idx < 0 {
		return ""
	}
	return strings.ReplaceAll(sourceURL[idx+len("/wiki/"):], "_", " ")
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

// Shelves are the Mongo collections the intel store keeps. The watcher
// tracks the wiki pages behind them.
var Shelves = []string{
	"units", "structures", "weapons", "stratagems", "armor", "boosters",
	"biomes", "planets", "missions", "difficulty", "effects",
}

// Backfill populates tracked_pages from existing docs that carry a source_url,
// so the very first poll can detect changes. Returns the number of rows seen.
//
// Only the crawled shelves are backfilled. The gear shelves come from the
// catalog, which does not know a page's revision id: tracking them here
// would make the next poll fetch every gear page at once — the burst that
// gets a client rate limited. A gear page joins tracked_pages when the
// watcher fetches it (one page at a time, at the rate the feed reports).
func (w *Watcher) Backfill(ctx context.Context) (int, error) {
	seen := 0
	for _, coll := range CrawlShelves {
		cur, err := w.DB.Collection(coll).Find(ctx, bson.M{},
			options.Find().SetProjection(bson.M{"source_url": 1, "rev_id": 1, "name": 1, "wiki_title": 1}))
		if err != nil {
			return seen, fmt.Errorf("watch: backfill %s: %w", coll, err)
		}
		for cur.Next(ctx) {
			var doc bson.M
			if err := cur.Decode(&doc); err != nil {
				log.Printf("watch: backfill %s: decode: %v", coll, err)
				continue
			}
			url, _ := doc["source_url"].(string)
			if !strings.Contains(url, "/wiki/") {
				continue
			}
			title := strings.TrimSpace(toString(doc["wiki_title"]))
			if title == "" {
				title = titleFromURL(url)
			}
			if title == "" {
				continue
			}
			rev := numInt(doc["rev_id"])
			res, err := w.DB.Collection("tracked_pages").UpdateOne(ctx,
				bson.M{"title": title},
				bson.M{"$set": bson.M{"url": url, "collection": coll, "rev_id": rev}},
				options.UpdateOne().SetUpsert(true))
			if err != nil {
				log.Printf("watch: backfill %s %s: %v", coll, title, err)
				continue
			}
			// Count only rows this run created: re-fetched rows report
			// MatchedCount, so restarts no longer overstate the log line.
			if res.UpsertedCount > 0 {
				seen++
			}
		}
		// A cursor error (a broken connection, a server hiccup) must not
		// read as "this shelf has no docs": the pages would look
		// untracked and the watcher would queue them all.
		if err := cur.Err(); err != nil {
			cur.Close(context.WithoutCancel(ctx))
			return seen, fmt.Errorf("watch: backfill %s: %w", coll, err)
		}
		if err := cur.Close(ctx); err != nil {
			log.Printf("watch: cursor close: %v", err)
		}
	}
	return seen, nil
}

// RunOnce polls and refreshes every changed page, then places any page the
// feed carried that the watcher had never seen. Returns the change count.
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
	if n, err := w.Discover(ctx); err != nil {
		log.Printf("watch: discover: %v", err)
	} else if n > 0 {
		log.Printf("watch: placed %d new page(s)", n)
	}
	return len(changes), nil
}

// Run polls on an interval until ctx is cancelled. A rate-limited wiki makes
// the poller wait longer than its interval: hammering a wiki that just said
// "slow down" is how a watcher gets shut out.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	for {
		wait := interval
		n, err := w.RunOnce(ctx)
		switch {
		case err != nil && wikifeed.IsRateLimit(err):
			wait = wikifeed.RetryDelay(err, w.backoff())
			log.Printf("watch: rate limited by the wiki, backing off for %s: %v", wait, err)
		case err != nil:
			log.Printf("watch: poll: %v", err)
		case n > 0:
			log.Printf("watch: refreshed %d page(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// backoff is the wait after a rate limit when the wiki gave no Retry-After.
func (w *Watcher) backoff() time.Duration {
	if w.Backoff > 0 {
		return w.Backoff
	}
	return 30 * time.Minute
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
