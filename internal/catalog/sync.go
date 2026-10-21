package catalog

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ciCollation matches names the way the unique name indexes do:
// case-insensitively, so a case-drifted name is the same doc and not a
// second one.
var ciCollation = &options.Collation{Locale: "en", Strength: 2}

// Fetch records how one collection was read.
type Fetch struct {
	Collection string
	// NotModified is true when the catalog answered 304: the snapshot is
	// unchanged, so the request cost nothing.
	NotModified bool
	// Cached is true when the payload came from the Mongo cache because the
	// fetch failed: freshness was lost, answers were not.
	Cached bool
	// Count is the number of items in the snapshot.
	Count int
	// Err carries the fetch failure, if any.
	Err string
}

// Result reports one shelf's sync.
type Result struct {
	// Shelf is the Mongo collection that was written ("" for context-only
	// collections such as traits).
	Shelf string
	// Sources are the catalog collections that fed this shelf.
	Sources []Fetch
	// Items is the number of catalog items considered.
	Items int
	// Updated counts docs whose catalog fields changed.
	Updated int
	// Unchanged counts docs already carrying the same catalog fields.
	Unchanged int
	// New counts docs the catalog added (no doc matched).
	New int
	// Removed counts docs the catalog no longer lists.
	Removed int
	// Mismatches counts docs whose curated fields disagree with the
	// catalog. The disagreement is reported, never applied.
	Mismatches int
	// Failed counts docs the sync could not write.
	Failed int
	// Err carries a shelf-level failure (the shelf was left untouched).
	Err string
}

// Options configures a sync.
type Options struct {
	DB     *mongo.Database
	Client *Client
	Cache  *Cache
	// Collections overrides the default collection list (tests).
	Collections []string
	// Now overrides the clock (tests).
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

func (o Options) collections() []string {
	if len(o.Collections) > 0 {
		return o.Collections
	}
	return Collections
}

// Sync refreshes the gear shelves from the catalog.
//
// The catalog is the source for gear: no wiki request is made, and an
// unchanged snapshot costs one 304 per collection. When a fetch fails the
// cached payload is used instead, so an outage costs freshness, not answers.
func Sync(ctx context.Context, opts Options) ([]Result, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("catalog sync: no database")
	}
	if opts.Client == nil {
		opts.Client = New()
	}
	cache := opts.Cache
	if cache == nil {
		cache = NewCache(opts.DB)
	}
	colls := opts.collections()

	// 1. Read every collection (conditionally, through the cache).
	snaps := map[string]Snapshot{}
	fetches := map[string]Fetch{}
	failed := 0
	for _, coll := range colls {
		f := Fetch{Collection: coll}
		cachedSnap, cachedETag, cachedOK, err := cache.Load(ctx, coll)
		if err != nil {
			log.Printf("catalog: cache read %s: %v", coll, err)
		}
		res, err := opts.Client.Fetch(ctx, coll, cachedETag)
		switch {
		case err != nil:
			f.Err = err.Error()
			failed++
			if cachedOK {
				// Freshness lost, answers kept.
				f.Cached = true
				snaps[coll] = cachedSnap
				f.Count = cachedSnap.Count()
				log.Printf("catalog: %s fetch failed, using cached snapshot: %v", coll, err)
			} else {
				log.Printf("catalog: %s fetch failed and nothing is cached: %v", coll, err)
			}
		case res.NotModified:
			f.NotModified = true
			if cachedOK {
				snaps[coll] = cachedSnap
				f.Count = cachedSnap.Count()
			} else {
				// A 304 without a cached payload cannot be served; ask
				// again without the validator.
				res2, err2 := opts.Client.Fetch(ctx, coll, "")
				if err2 != nil {
					f.Err = err2.Error()
					failed++
					break
				}
				if res2.NotModified {
					// The catalog says "unchanged" but there is nothing
					// to serve: an empty snapshot would report every doc
					// as removed.
					f.Err = "catalog answered 304 without a cached snapshot"
					failed++
					break
				}
				// The payload came down: this was not a free 304.
				f.NotModified = false
				snaps[coll] = res2.Snapshot
				f.Count = res2.Snapshot.Count()
				if err := cache.Save(ctx, coll, res2.Snapshot, res2.ETag); err != nil {
					log.Printf("catalog: cache write %s: %v", coll, err)
				}
			}
		default:
			snaps[coll] = res.Snapshot
			f.Count = res.Snapshot.Count()
			if err := cache.Save(ctx, coll, res.Snapshot, res.ETag); err != nil {
				log.Printf("catalog: cache write %s: %v", coll, err)
			}
		}
		fetches[coll] = f
	}

	// 2. Build the context the gear fields reference (traits, passives,
	// warbonds). These collections feed no shelf of their own.
	info := buildContext(snaps)

	// 3. Apply the shelves.
	results := []Result{}
	for _, shelf := range shelfOrder(colls) {
		sources := []Fetch{}
		items := []Item{}
		version := ""
		loaded := false
		for _, coll := range colls {
			if ShelfForCollection(coll) != shelf {
				continue
			}
			sources = append(sources, fetches[coll])
			items = append(items, snaps[coll].Items...)
			if version == "" {
				version = snaps[coll].Version
			}
			if _, ok := snaps[coll]; ok {
				loaded = true
			}
		}
		if len(sources) == 0 {
			continue
		}
		res := Result{Shelf: shelf, Sources: sources, Items: len(items)}
		if !loaded {
			// Every source of this shelf failed and nothing was cached:
			// applying an empty item list would report every doc on the
			// shelf as removed. Leave the shelf alone.
			res.Err = "no catalog snapshot for this shelf: it was left untouched"
			results = append(results, res)
			continue
		}
		merged := mergeItems(items)
		res.Items = len(merged)
		st, err := applyShelf(ctx, opts.DB, shelf, merged, info, version, opts.now())
		res.Updated, res.Unchanged, res.New, res.Removed, res.Mismatches = st.updated, st.unchanged, st.new, st.removed, st.mismatches
		res.Failed = st.failed
		if err != nil {
			// The shelf failed: report it instead of dropping the line.
			res.Err = err.Error()
			results = append(results, res)
			return results, err
		}
		results = append(results, res)
	}
	// Context-only collections still report their fetch status.
	for _, coll := range colls {
		if ShelfForCollection(coll) != "" {
			continue
		}
		results = append(results, Result{Shelf: "", Sources: []Fetch{fetches[coll]}, Items: fetches[coll].Count})
	}
	if len(snaps) == 0 && failed > 0 {
		// Every collection failed and nothing was cached: the catalog is
		// down and the seeds are the only intel left. Say so.
		return results, fmt.Errorf("catalog: every collection failed to load")
	}
	return results, nil
}

// shelfOrder lists the shelves in a stable order.
func shelfOrder(colls []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, coll := range colls {
		shelf := ShelfForCollection(coll)
		if shelf == "" || seen[shelf] {
			continue
		}
		seen[shelf] = true
		out = append(out, shelf)
	}
	sort.Strings(out)
	return out
}

// mergeItems folds the collections that feed one shelf into a single item
// list: the first collection to carry a field wins, later ones only fill
// what is still empty. The armor shelf is fed by armors, helmets and
// armor-sets, which describe the same page from three angles.
func mergeItems(items []Item) []Item {
	byKey := map[string]int{}
	out := []Item{}
	for _, it := range items {
		key := itemKey(it)
		if key == "" {
			continue
		}
		idx, ok := byKey[key]
		if !ok {
			byKey[key] = len(out)
			out = append(out, it)
			continue
		}
		out[idx] = mergeItem(out[idx], it)
	}
	return out
}

// itemKey is the identity of an item across the collections that describe
// it: the wiki page it came from, else its name.
func itemKey(it Item) string {
	if it.Wiki.Title != "" {
		return normalize(it.Wiki.Title)
	}
	return normalize(it.Name)
}

func mergeItem(dst, src Item) Item {
	if dst.Description == "" {
		dst.Description = src.Description
	}
	if dst.Wiki.Title == "" {
		dst.Wiki.Title = src.Wiki.Title
	}
	if dst.Wiki.URL == "" {
		dst.Wiki.URL = src.Wiki.URL
	}
	if len(dst.Wiki.Flags) == 0 {
		dst.Wiki.Flags = src.Wiki.Flags
	}
	if len(dst.Aliases) == 0 {
		dst.Aliases = src.Aliases
	}
	if dst.Subcategory == "" {
		dst.Subcategory = src.Subcategory
	}
	if dst.Weight == "" {
		dst.Weight = src.Weight
	}
	if dst.ArmorRating == nil {
		dst.ArmorRating = src.ArmorRating
	}
	if dst.Speed == nil {
		dst.Speed = src.Speed
	}
	if dst.StaminaRegen == nil {
		dst.StaminaRegen = src.StaminaRegen
	}
	if dst.PassiveID == "" {
		dst.PassiveID = src.PassiveID
	}
	if dst.ArmorID == "" {
		dst.ArmorID = src.ArmorID
	}
	if dst.HelmetID == "" {
		dst.HelmetID = src.HelmetID
	}
	if dst.CapeID == "" {
		dst.CapeID = src.CapeID
	}
	dst.SetIDs = unionStrings(dst.SetIDs, src.SetIDs)
	return dst
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range append(append([]string{}, a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// contextInfo holds the cross-collection names the gear fields reference.
type contextInfo struct {
	traitNames   map[string]string
	passiveNames map[string]string
	passiveFx    map[string][]string
	warbondNames map[string]string
}

func buildContext(snaps map[string]Snapshot) contextInfo {
	info := contextInfo{
		traitNames:   map[string]string{},
		passiveNames: map[string]string{},
		passiveFx:    map[string][]string{},
		warbondNames: map[string]string{},
	}
	for _, it := range snaps["weapon-traits"].Items {
		info.traitNames[it.ID] = it.Name
	}
	for _, it := range snaps["passives"].Items {
		info.passiveNames[it.ID] = it.Name
		info.passiveFx[it.ID] = it.Effects
	}
	for _, it := range snaps["warbonds"].Items {
		info.warbondNames[it.ID] = it.Name
	}
	return info
}

// stats counts one shelf's write outcomes.
type stats struct {
	updated    int
	unchanged  int
	new        int
	removed    int
	mismatches int
	failed     int
}

// applyShelf writes one shelf's items into Mongo.
func applyShelf(ctx context.Context, db *mongo.Database, shelf string, items []Item, info contextInfo, version string, now time.Time) (stats, error) {
	var st stats
	docs, err := loadShelf(ctx, db, shelf)
	if err != nil {
		return st, err
	}
	index := map[string]*bson.M{}
	for i := range docs {
		doc := &docs[i]
		for _, key := range docKeys(*doc) {
			if _, ok := index[key]; !ok {
				index[key] = doc
			}
		}
	}
	seen := map[string]bool{}
	for _, it := range items {
		if strings.TrimSpace(it.Name) == "" {
			// A nameless item would land as a doc with an empty name.
			st.failed++
			log.Printf("catalog: %s: skipping an item with no name", shelf)
			continue
		}
		set := catalogFields(it, info, version, now)
		// The armor and booster shelves have no catalog category of their
		// own; name the shelf so the roster can still say what a doc is.
		if _, ok := set["category"]; !ok {
			switch shelf {
			case "armor", "boosters":
				set["category"] = shelf
			}
		}
		if shelf == "armor" && it.Weight != "" {
			set["subcategory"] = it.Weight
		}
		doc := findDoc(index, it)
		if doc == nil {
			seed := bson.M{}
			for k, v := range set {
				seed[k] = v
			}
			seed["name"] = it.Name
			label := sourceLabel(it)
			if label == "" {
				label = "catalog"
			}
			seed["source"] = label
			seed["rev_id"] = 0
			seed["patch_version"] = ""
			if len(it.Aliases) > 0 {
				seed["aliases"] = it.Aliases
			}
			if it.Description != "" {
				seed["role"] = it.Description
				seed["description"] = it.Description
			}
			if params := paramsFromStats(it.StatsRaw); len(params) > 0 {
				seed["params"] = params
			}
			_, err := db.Collection(shelf).UpdateOne(ctx,
				bson.M{"name": it.Name},
				bson.M{"$set": seed},
				options.UpdateOne().SetUpsert(true).SetCollation(ciCollation))
			if err != nil {
				// One bad write must not cost the whole shelf.
				st.failed++
				log.Printf("catalog: %s %s: %v", shelf, it.Name, err)
				continue
			}
			st.new++
			continue
		}
		seen[docKey(*doc)] = true
		mismatch := diffCurated(*doc, it)
		if len(mismatch) > 0 {
			set["catalog_mismatches"] = mismatch
			st.mismatches++
		}
		res, err := db.Collection(shelf).UpdateOne(ctx,
			bson.M{"_id": (*doc)["_id"]},
			bson.M{"$set": set})
		if err != nil {
			// One bad write must not cost the whole shelf.
			st.failed++
			log.Printf("catalog: %s %s: %v", shelf, it.Name, err)
			continue
		}
		if res.ModifiedCount > 0 {
			st.updated++
		} else {
			st.unchanged++
		}
	}
	// Docs the catalog used to list and no longer does are reported, not
	// deleted: a curated seed is the floor, and a catalog gap is not proof
	// that the item is gone from the game.
	for i := range docs {
		doc := docs[i]
		if seen[docKey(doc)] {
			continue
		}
		if _, ok := doc["catalog_id"]; !ok {
			continue
		}
		st.removed++
		log.Printf("catalog: %s %q is no longer in the catalog (kept, not deleted)", shelf, toString(doc["name"]))
	}
	return st, nil
}

// docKey identifies a doc by its _id, whatever shape it has.
func docKey(doc bson.M) string {
	switch t := doc["_id"].(type) {
	case bson.ObjectID:
		return t.Hex()
	case string:
		return t
	case nil:
		return "name:" + toString(doc["name"])
	}
	return fmt.Sprint(doc["_id"])
}

// loadShelf reads every doc of a shelf, sorted by name: the alias index
// resolves the first match, and an unsorted read would make that pick
// random.
func loadShelf(ctx context.Context, db *mongo.Database, shelf string) ([]bson.M, error) {
	cur, err := db.Collection(shelf).Find(ctx, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("catalog load %s: %w", shelf, err)
	}
	defer cur.Close(context.WithoutCancel(ctx))
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("catalog load %s: %w", shelf, err)
	}
	sort.Slice(docs, func(i, j int) bool {
		return toString(docs[i]["name"]) < toString(docs[j]["name"])
	})
	return docs, nil
}

// docKeys are the names a doc can be matched by: its name, its aliases, the
// wiki page it came from and that page's URL.
func docKeys(doc bson.M) []string {
	keys := []string{}
	if n := normalize(toString(doc["name"])); n != "" {
		keys = append(keys, n)
	}
	for _, a := range stringList(doc, "aliases") {
		if n := normalize(a); n != "" {
			keys = append(keys, n)
		}
	}
	if t := normalize(toString(doc["wiki_title"])); t != "" {
		keys = append(keys, t)
	}
	if u := normalize(toString(doc["source_url"])); u != "" {
		keys = append(keys, u)
	}
	return keys
}

// findDoc matches a catalog item to a doc: by name, by alias, by the wiki
// title the item was scraped from, or by that page's URL.
func findDoc(index map[string]*bson.M, it Item) *bson.M {
	candidates := []string{it.Name, it.Wiki.Title, it.Wiki.URL}
	candidates = append(candidates, it.Aliases...)
	for _, c := range candidates {
		key := normalize(c)
		if key == "" {
			continue
		}
		if doc, ok := index[key]; ok {
			return doc
		}
	}
	return nil
}

// Index answers which shelf a wiki page belongs to, from the cached
// catalog. The catalog knows every gear item's page, so a new gear page
// needs no category guesswork — and it never calls the catalog API.
type Index struct {
	shelves map[string]string
}

// LoadIndex reads the cached catalog once and maps every gear page to its
// shelf. One pass per discovery run, not one parse per unseen page.
func LoadIndex(ctx context.Context, db *mongo.Database) *Index {
	idx := &Index{shelves: map[string]string{}}
	if db == nil {
		return idx
	}
	cache := NewCache(db)
	for _, coll := range Collections {
		shelf := ShelfForCollection(coll)
		if shelf == "" {
			continue
		}
		snap, _, ok, err := cache.Load(ctx, coll)
		if err != nil {
			log.Printf("catalog: index read %s: %v", coll, err)
			continue
		}
		if !ok {
			continue
		}
		for _, it := range snap.Items {
			for _, key := range itemKeys(it) {
				if _, exists := idx.shelves[key]; !exists {
					idx.shelves[key] = shelf
				}
			}
		}
	}
	return idx
}

// itemKeys are the names a catalog item can be found by.
func itemKeys(it Item) []string {
	keys := []string{}
	add := func(s string) {
		if n := normalize(s); n != "" {
			keys = append(keys, n)
		}
	}
	add(it.Name)
	add(it.Wiki.Title)
	for _, a := range it.Aliases {
		add(a)
	}
	return keys
}

// Shelf returns the shelf a page belongs to.
func (i *Index) Shelf(title string) (string, bool) {
	if i == nil {
		return "", false
	}
	shelf, ok := i.shelves[normalize(title)]
	return shelf, ok
}

// LookupShelf answers which shelf a wiki page belongs to, using the cached
// catalog. It never calls the catalog API.
func LookupShelf(ctx context.Context, db *mongo.Database, title string) (string, bool) {
	return LoadIndex(ctx, db).Shelf(title)
}
