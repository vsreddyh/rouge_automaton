package wikiwatch

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"rouge_automaton/internal/catalog"
)

// Factions are the wiki's faction categories and the repo's faction field.
var Factions = map[string]string{
	"automatons": "automatons",
	"terminids":  "terminids",
	"illuminate": "illuminate",
}

// ShelfForCategories maps the wiki's own categories to a repo shelf.
//
// The type category decides, never the generic "Weapons" category: that one
// sits on primaries, secondaries and throwables *and* on the support-weapon
// stratagem, so a page carrying only "Weapons" is ambiguous and needs a
// second signal (the catalog).
func ShelfForCategories(cats []string) string {
	lower := map[string]bool{}
	for _, c := range cats {
		lower[strings.ToLower(strings.TrimSpace(c))] = true
	}
	// Stratagem types: "Stratagems" and its subtypes ("Support Weapon
	// Stratagems", "Sentry Stratagems", "Orbital Stratagems").
	for c := range lower {
		if c == "stratagems" || strings.HasSuffix(c, " stratagems") {
			return "stratagems"
		}
	}
	// Weapon types.
	for _, c := range []string{"primary weapons", "secondary weapons", "throwables"} {
		if lower[c] {
			return "weapons"
		}
	}
	// Armor.
	for _, c := range []string{"armor", "light armor", "heavy armor", "helmets"} {
		if lower[c] {
			return "armor"
		}
	}
	// Enemies: a faction plus an enemy category.
	for _, c := range []string{"enemies", "small enemies", "medium enemies", "large enemies"} {
		if lower[c] && FactionOf(cats) != "" {
			return "units"
		}
	}
	// Structures.
	if lower["structures"] {
		return "structures"
	}
	// The world shelves, in a fixed order.
	for _, pair := range []struct{ cat, shelf string }{
		{"biomes", "biomes"},
		{"planets", "planets"},
		{"missions", "missions"},
		{"difficulty", "difficulty"},
		{"effects", "effects"},
	} {
		if lower[pair.cat] {
			return pair.shelf
		}
	}
	// The generic "Weapons" category decides nothing on its own.
	return ""
}

// FactionOf returns the faction a page's categories name, or "".
func FactionOf(cats []string) string {
	for _, c := range cats {
		if f, ok := Factions[strings.ToLower(strings.TrimSpace(c))]; ok {
			return f
		}
	}
	return ""
}

// Discover places the pages the feed carried that the watcher had never
// seen. A new page is routed to a shelf, fetched, written and registered, so
// the next edit to it refreshes through the normal path.
//
// A page that matches no shelf (a guide, a patch note) is ignored, not
// filed. Returns the number of pages placed.
func (w *Watcher) Discover(ctx context.Context) (int, error) {
	if len(w.untracked) == 0 {
		return 0, nil
	}
	titles := w.untracked
	w.untracked = nil
	limit := w.DiscoverLimit
	if limit <= 0 {
		limit = 5
	}
	placed := 0
	attempts := 0
	for i, title := range titles {
		if attempts >= limit {
			// Keep the rest for the next poll: every attempt costs a wiki
			// request, and the wiki counts requests.
			w.requeue(titles[i:])
			break
		}
		attempts++
		ok, err := w.discoverOne(ctx, title)
		if err != nil {
			log.Printf("watch: discover %s: %v", title, err)
			continue
		}
		if ok {
			placed++
			continue
		}
		// The page matched no shelf: stop spending requests on it.
		w.ignore(title)
	}
	return placed, nil
}

// requeue puts the pages the discovery pass did not reach back on the
// queue, minus the ones already known to match no shelf.
func (w *Watcher) requeue(titles []string) {
	for _, t := range titles {
		if w.ignored[t] {
			continue
		}
		w.untracked = append(w.untracked, t)
	}
}

// discoverOne routes one unseen page, fetches it and files it.
func (w *Watcher) discoverOne(ctx context.Context, title string) (bool, error) {
	// A page the store already knows (a gear doc the catalog synced, a
	// seed that was never tracked) needs no wiki call to place.
	shelf, ok := w.shelfForDoc(ctx, title)
	if !ok {
		// Gear is routed from the catalog: it knows every gear page, and
		// it needs no wiki call to say so.
		shelf, ok = catalog.LookupShelf(ctx, w.DB, title)
	}
	if !ok {
		// The wiki's own categories are the one thing the catalog cannot
		// answer: units and the world.
		info, err := w.Feed.Categories(ctx, title)
		if err != nil {
			return false, err
		}
		if info.Missing {
			return false, nil
		}
		shelf = ShelfForCategories(info.Categories)
		if shelf == "" {
			// Nothing matched a shelf: this is a guide or a patch note.
			log.Printf("watch: ignoring %s (no shelf matches its categories)", title)
			return false, nil
		}
		faction := FactionOf(info.Categories)
		return w.store(ctx, shelf, title, faction)
	}
	return w.store(ctx, shelf, title, "")
}

// shelfForDoc finds the shelf a doc with this name already lives on. It
// costs Mongo lookups, not wiki requests.
func (w *Watcher) shelfForDoc(ctx context.Context, title string) (string, bool) {
	if w.DB == nil {
		return "", false
	}
	for _, shelf := range Shelves {
		count, err := w.DB.Collection(shelf).CountDocuments(ctx,
			bson.M{"name": title}, options.Count().SetLimit(1))
		if err == nil && count > 0 {
			return shelf, true
		}
	}
	return "", false
}

// store fetches a page, writes the doc (creating it when the shelf has no
// doc for it) and registers the page for future refreshes.
func (w *Watcher) store(ctx context.Context, shelf, title, faction string) (bool, error) {
	page, err := w.Feed.Page(ctx, title)
	if err != nil {
		return false, err
	}
	if page.RevID == 0 {
		return false, nil
	}
	canonical := page.Title
	if canonical == "" {
		canonical = title
	}
	sourceURL := w.sourceURL(canonical)
	cur, err := w.findDoc(ctx, shelf, canonical)
	if err != nil {
		return false, err
	}
	set := w.buildFields(shelf, page, cur, sourceURL)
	if cur == nil {
		// A page the store has never seen: the wiki-owned fields are
		// written, the curated ones stay empty until someone curates
		// them.
		set["name"] = canonical
		set["source"] = "helldivers.wiki.gg"
		if faction != "" {
			set["faction"] = faction
		}
		if _, ok := set["upcoming"]; !ok {
			set["upcoming"] = false
			set["live"] = true
		}
		if _, err := w.DB.Collection(shelf).UpdateOne(ctx,
			bson.M{"name": canonical},
			bson.M{"$set": set},
			options.UpdateOne().SetUpsert(true)); err != nil {
			return false, err
		}
		log.Printf("watch: new %s %s (%s)", shelf, canonical, sourceURL)
	} else {
		if _, err := w.DB.Collection(shelf).UpdateOne(ctx,
			bson.M{"_id": cur["_id"]},
			bson.M{"$set": set}); err != nil {
			return false, err
		}
	}
	if err := w.track(ctx, canonical, sourceURL, shelf, page.RevID); err != nil {
		return false, err
	}
	return true, nil
}

// findDoc looks for a doc on a shelf by name (case-insensitively) or by the
// wiki page it came from.
func (w *Watcher) findDoc(ctx context.Context, shelf, title string) (bson.M, error) {
	coll := w.DB.Collection(shelf)
	var doc bson.M
	err := coll.FindOne(ctx, bson.M{"name": title}).Decode(&doc)
	if err == nil {
		return doc, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return nil, err
	}
	// The seed's names are not always spelled exactly like the wiki's
	// titles, so fall back to a case-insensitive match.
	re := regexp.MustCompile("^" + regexp.QuoteMeta(title) + "$")
	err = coll.FindOne(ctx, bson.M{"name": bson.M{"$regex": re.String(), "$options": "i"}}).Decode(&doc)
	if err == nil {
		return doc, nil
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return nil, err
}
