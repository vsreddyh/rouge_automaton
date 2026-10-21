package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// curatedFields never come from the catalog: they are the seed job's work
// (weak points, counters, aliases, role, params). A sync reports a
// disagreement instead of overwriting one of these.
var curatedFields = map[string]bool{
	"weak_points": true, "counters": true, "aliases": true,
	"role": true, "params": true,
}

// CuratedField reports whether a field belongs to the curated seed set.
func CuratedField(name string) bool { return curatedFields[name] }

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
					break
				}
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
		for _, coll := range colls {
			if ShelfForCollection(coll) != shelf {
				continue
			}
			sources = append(sources, fetches[coll])
			items = append(items, snaps[coll].Items...)
			if version == "" {
				version = snaps[coll].Version
			}
		}
		if len(sources) == 0 {
			continue
		}
		merged := mergeItems(items)
		res := Result{Shelf: shelf, Sources: sources, Items: len(merged)}
		st, err := applyShelf(ctx, opts.DB, shelf, merged, info, version, opts.now())
		res.Updated, res.Unchanged, res.New, res.Removed, res.Mismatches = st.updated, st.unchanged, st.new, st.removed, st.mismatches
		if err != nil {
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
	seen := map[any]bool{}
	for _, it := range items {
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
				options.UpdateOne().SetUpsert(true))
			if err != nil {
				return st, fmt.Errorf("catalog %s %s: %w", shelf, it.Name, err)
			}
			st.new++
			continue
		}
		seen[(*doc)["_id"]] = true
		mismatch := diffCurated(*doc, it)
		if len(mismatch) > 0 {
			set["catalog_mismatches"] = mismatch
			st.mismatches++
		}
		res, err := db.Collection(shelf).UpdateOne(ctx,
			bson.M{"_id": (*doc)["_id"]},
			bson.M{"$set": set})
		if err != nil {
			return st, fmt.Errorf("catalog %s %s: %w", shelf, it.Name, err)
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
		if seen[doc["_id"]] {
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

// loadShelf reads every doc of a shelf.
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

// catalogFields are the fields the catalog owns. Curated seed fields are
// never in this map.
func catalogFields(it Item, info contextInfo, version string, now time.Time) bson.M {
	set := bson.M{
		"catalog_id":         it.ID,
		"catalog_fetched_at": now,
		"upcoming":           it.Upcoming,
		"live":               !it.Upcoming,
	}
	if version != "" {
		set["catalog_version"] = version
	}
	if it.Wiki.Title != "" {
		set["wiki_title"] = it.Wiki.Title
	}
	if it.Wiki.URL != "" {
		set["wiki_url"] = it.Wiki.URL
		// The wiki page is the provenance the seeds never had.
		set["source_url"] = it.Wiki.URL
	}
	if len(it.Wiki.Flags) > 0 {
		set["wiki_flags"] = it.Wiki.Flags
	}
	if it.Category != "" {
		set["category"] = it.Category
	}
	if it.Subcategory != "" {
		set["subcategory"] = it.Subcategory
	}
	if it.Kind != "" {
		set["kind"] = it.Kind
	}
	if it.Description != "" {
		set["catalog_description"] = it.Description
	}
	if len(it.TraitIDs) > 0 {
		set["trait_ids"] = it.TraitIDs
		names := make([]string, 0, len(it.TraitIDs))
		for _, id := range it.TraitIDs {
			if n, ok := info.traitNames[id]; ok && n != "" {
				names = append(names, n)
				continue
			}
			names = append(names, id)
		}
		set["traits"] = names
	}
	if len(it.Firearm) > 0 {
		set["firearm"] = toBsonM(it.Firearm)
	}
	if len(it.Throwable) > 0 {
		set["throwable"] = toBsonM(it.Throwable)
	}
	if len(it.Melee) > 0 {
		set["melee"] = toBsonM(it.Melee)
	}
	if len(it.Attacks) > 0 {
		set["attacks"] = bsonList(it.Attacks)
	}
	if len(it.StatsRaw) > 0 {
		set["stats_raw"] = toBsonM(it.StatsRaw)
	}
	if len(it.Source) > 0 {
		set["catalog_source"] = toBsonM(it.Source)
	}
	if label := sourceLabel(it); label != "" {
		set["warbond"] = label
	}
	// Stratagems.
	if it.PermitType != "" {
		set["permit_type"] = it.PermitType
	}
	if it.Availability != "" {
		set["availability"] = it.Availability
	}
	if len(it.Code) > 0 {
		set["code"] = it.Code
	}
	if it.CooldownS != nil {
		set["cooldown_s"] = it.CooldownS
	}
	if it.CallInTimeS != nil {
		set["call_in_time_s"] = it.CallInTimeS
	}
	if it.CallInTimeUpgradedS != nil {
		set["call_in_time_upgraded_s"] = it.CallInTimeUpgradedS
	}
	if it.Uses != nil {
		set["uses"] = it.Uses
	}
	if it.UnlockLevel != nil {
		set["unlock_level"] = it.UnlockLevel
	}
	if len(it.ShipModules) > 0 {
		set["ship_modules"] = bsonList(it.ShipModules)
	}
	if len(it.SupportWeapon) > 0 {
		set["support_weapon"] = toBsonM(it.SupportWeapon)
	}
	if len(it.Backpack) > 0 {
		set["backpack"] = toBsonM(it.Backpack)
	}
	if len(it.CooldownVariants) > 0 {
		vals := make([]any, 0, len(it.CooldownVariants))
		for _, v := range it.CooldownVariants {
			vals = append(vals, v)
		}
		set["cooldown_variants"] = bsonList(vals)
	}
	// Armor.
	if it.Weight != "" {
		set["weight"] = it.Weight
	}
	if it.ArmorRating != nil {
		set["armor_rating"] = it.ArmorRating
	}
	if it.Speed != nil {
		set["speed"] = it.Speed
	}
	if it.StaminaRegen != nil {
		set["stamina_regen"] = it.StaminaRegen
	}
	if it.PassiveID != "" {
		set["passive_id"] = it.PassiveID
		if n, ok := info.passiveNames[it.PassiveID]; ok && n != "" {
			set["passive"] = n
		}
		if fx := info.passiveFx[it.PassiveID]; len(fx) > 0 {
			set["passive_effects"] = fx
		}
	}
	if len(it.SetIDs) > 0 {
		set["set_ids"] = it.SetIDs
	}
	if it.ArmorID != "" {
		set["armor_id"] = it.ArmorID
	}
	if it.HelmetID != "" {
		set["helmet_id"] = it.HelmetID
	}
	if it.CapeID != "" {
		set["cape_id"] = it.CapeID
	}
	// Boosters.
	if it.Effect != "" {
		set["effect"] = it.Effect
	}
	if len(it.Effects) > 0 {
		set["effects"] = it.Effects
	}
	return set
}

// sourceLabel renders where an item comes from ("Ironclad Democracy P1").
func sourceLabel(it Item) string {
	if len(it.Source) == 0 {
		return ""
	}
	if label, ok := it.Source["label"].(string); ok && label != "" {
		return label
	}
	if typ, ok := it.Source["type"].(string); ok && typ != "" {
		return typ
	}
	return ""
}

// Mismatch is one disagreement between a curated seed field and the catalog.
type Mismatch struct {
	Field   string `bson:"field"`
	Curated string `bson:"curated"`
	Catalog string `bson:"catalog"`
}

var numberRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// diffCurated reports where the curated seed and the catalog disagree. The
// curated value wins; the difference is recorded, never applied.
func diffCurated(doc bson.M, it Item) []Mismatch {
	out := []Mismatch{}
	params, ok := paramBag(doc)
	if !ok {
		return out
	}
	// Damage per second: the catalog carries both firing modes, a seed
	// usually carries one.
	if len(it.Firearm) > 0 {
		if dps, ok := it.Firearm["dps"].([]any); ok && len(dps) > 0 {
			curated := toString(params["dps"])
			have := numbersIn(curated)
			missing := []string{}
			for _, d := range dps {
				n, ok := toFloat(d)
				if !ok {
					continue
				}
				if !containsNumber(have, n) {
					missing = append(missing, trimNumber(n))
				}
			}
			if len(missing) > 0 {
				out = append(out, Mismatch{
					Field:   "params.dps",
					Curated: curated,
					Catalog: strings.Join(missing, ", "),
				})
			}
		}
	}
	// Armor numbers.
	for _, pair := range []struct{ key, field string }{
		{"armor", "armor_rating"},
		{"speed", "speed"},
		{"stam_regen", "stamina_regen"},
	} {
		curated := toString(params[pair.key])
		if curated == "" {
			continue
		}
		var catalogVal any
		switch pair.field {
		case "armor_rating":
			catalogVal = it.ArmorRating
		case "speed":
			catalogVal = it.Speed
		case "stamina_regen":
			catalogVal = it.StaminaRegen
		}
		if catalogVal == nil {
			continue
		}
		catNum, ok := toFloat(catalogVal)
		if !ok {
			continue
		}
		curNum, ok := toFloat(curated)
		if !ok {
			continue
		}
		if curNum != catNum {
			out = append(out, Mismatch{
				Field:   "params." + pair.key,
				Curated: curated,
				Catalog: trimNumber(catNum),
			})
		}
	}
	return out
}

// paramBag reads a doc's nested params document in any writer shape: the
// same doc can come from a seed, from a crawl or from a catalog sync.
func paramBag(doc bson.M) (map[string]any, bool) {
	switch t := doc["params"].(type) {
	case bson.M:
		return map[string]any(t), true
	case map[string]any:
		return t, true
	case bson.D:
		out := map[string]any{}
		for _, e := range t {
			out[e.Key] = e.Value
		}
		return out, true
	}
	return nil, false
}

// numbersIn pulls the numbers out of a prose value.
func numbersIn(s string) []float64 {
	var out []float64
	for _, m := range numberRe.FindAllString(s, -1) {
		if n, err := strconv.ParseFloat(strings.ReplaceAll(m, ",", ""), 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func containsNumber(list []float64, want float64) bool {
	for _, n := range list {
		if n == want {
			return true
		}
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(t), ",", ""), 64)
		return f, err == nil
	}
	return 0, false
}

func trimNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// paramsFromStats turns the catalog's raw stat table into a params bag, so
// a doc the catalog creates answers with the same shape as a seeded one.
func paramsFromStats(stats map[string]any) bson.M {
	out := bson.M{}
	for k, v := range stats {
		key := paramKey(k)
		if key == "" {
			continue
		}
		out[key] = toString(v)
	}
	return out
}

// paramKey folds a stat label into a params key ("Standard Damage" ->
// "standard_damage").
func paramKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	key := strings.Trim(b.String(), "_")
	for strings.Contains(key, "__") {
		key = strings.ReplaceAll(key, "__", "_")
	}
	return key
}

// toString renders a BSON value as one line of text.
func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return trimNumber(t)
	case float32:
		return trimNumber(float64(t))
	case int:
		return strconv.Itoa(t)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return fmt.Sprint(v)
}

// stringList reads a list field in any writer shape.
func stringList(doc bson.M, key string) []string {
	switch t := doc[key].(type) {
	case bson.A:
		var out []string
		for _, e := range t {
			if s := toString(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case []any:
		var out []string
		for _, e := range t {
			if s := toString(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

// toBsonM converts a decoded JSON map into a BSON-safe document, so the
// catalog's nested objects land as documents instead of opaque blobs.
func toBsonM(m map[string]any) bson.M {
	out := bson.M{}
	for k, v := range m {
		out[k] = bsonValue(v)
	}
	return out
}

// bsonList converts a decoded JSON list into a BSON-safe array, so nested
// documents land as documents instead of opaque blobs.
func bsonList(items []any) bson.A {
	arr := make(bson.A, 0, len(items))
	for _, item := range items {
		arr = append(arr, bsonValue(item))
	}
	return arr
}

func bsonValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return toBsonM(t)
	case []any:
		arr := make(bson.A, 0, len(t))
		for _, item := range t {
			arr = append(arr, bsonValue(item))
		}
		return arr
	default:
		return v
	}
}

// LookupShelf answers which shelf a wiki page belongs to, using the cached
// catalog: the catalog knows every gear item's page, so a new gear page
// needs no category guesswork. It never calls the catalog API.
func LookupShelf(ctx context.Context, db *mongo.Database, title string) (string, bool) {
	if db == nil || strings.TrimSpace(title) == "" {
		return "", false
	}
	want := normalize(title)
	cache := NewCache(db)
	for _, coll := range Collections {
		shelf := ShelfForCollection(coll)
		if shelf == "" {
			continue
		}
		snap, _, ok, err := cache.Load(ctx, coll)
		if err != nil || !ok {
			continue
		}
		for _, it := range snap.Items {
			if normalize(it.Name) == want || normalize(it.Wiki.Title) == want {
				return shelf, true
			}
			for _, a := range it.Aliases {
				if normalize(a) == want {
					return shelf, true
				}
			}
		}
	}
	return "", false
}
