package wikiwatch

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"rouge_automaton/internal/wikifeed"
)

// CrawlShelves are the shelves the crawler refreshes from the wiki: the
// units and the world. Gear is not crawled — the catalog owns it, and the
// wiki is asked about gear only when its feed says a gear page changed.
var CrawlShelves = []string{
	"units", "structures", "biomes", "planets", "missions", "difficulty", "effects",
}

// CrawlResult reports one shelf's crawl.
type CrawlResult struct {
	// Shelf is the Mongo collection that was crawled.
	Shelf string
	// Pages is the number of wiki pages fetched.
	Pages int
	// Updated counts docs whose wiki fields changed.
	Updated int
	// Unchanged counts docs already carrying the same revision.
	Unchanged int
	// Missing counts pages the wiki does not have (a seed name with no
	// wiki page yet).
	Missing int
	// Failed counts pages the wiki refused to answer.
	Failed int
}

// Crawl refreshes the unit and world shelves from the wiki, one page per
// shelf member. It is a slow, polite refresh (a crawl a month is ~70
// pages), and it is idempotent: a second run over unchanged pages writes
// nothing.
//
// The field mapping is the one the watcher already uses, so a crawled doc
// and a refreshed doc look identical, and the curated seed fields survive.
func (w *Watcher) Crawl(ctx context.Context) ([]CrawlResult, error) {
	results := []CrawlResult{}
	for _, shelf := range CrawlShelves {
		res := CrawlResult{Shelf: shelf}
		docs, err := w.shelfDocs(ctx, shelf)
		if err != nil {
			return results, err
		}
		fetched := 0
		for _, doc := range docs {
			if w.CrawlLimit > 0 && fetched >= w.CrawlLimit {
				log.Printf("watch: crawl %s stopped at the %d page cap", shelf, w.CrawlLimit)
				break
			}
			title := w.docTitle(doc)
			if title == "" {
				continue
			}
			// The cap counts attempts, not successes: a wiki that keeps
			// failing is exactly when the crawl must stop asking.
			fetched++
			res.Pages++
			page, err := w.Feed.Page(ctx, title)
			if err != nil {
				res.Failed++
				log.Printf("watch: crawl %s %s: %v", shelf, title, err)
				continue
			}
			if w.CrawlDelay > 0 {
				select {
				case <-ctx.Done():
					return results, ctx.Err()
				case <-time.After(w.CrawlDelay):
				}
			}
			if page.RevID == 0 {
				res.Missing++
				log.Printf("watch: crawl %s %s: no wiki page", shelf, title)
				continue
			}
			canonical := page.Title
			if canonical == "" {
				canonical = title
			}
			sourceURL := w.sourceURL(canonical)
			// A second run over unchanged pages must write nothing.
			if numInt(doc["rev_id"]) == page.RevID && toString(doc["source_url"]) == sourceURL {
				res.Unchanged++
				continue
			}
			set := w.buildFields(shelf, page, doc, sourceURL)
			if _, err := w.DB.Collection(shelf).UpdateOne(ctx,
				bson.M{"_id": doc["_id"]},
				bson.M{"$set": set}); err != nil {
				// One bad write must not cost the rest of the crawl.
				res.Failed++
				log.Printf("watch: crawl %s %s: %v", shelf, title, err)
				continue
			}
			if err := w.track(ctx, canonical, sourceURL, shelf, page.RevID); err != nil {
				log.Printf("watch: track %s: %v", canonical, err)
			}
			res.Updated++
		}
		results = append(results, res)
	}
	return results, nil
}

// shelfDocs reads a shelf's docs. The whole doc is loaded: the field
// mapping reads the curated fields (role, counters, params) to keep them.
func (w *Watcher) shelfDocs(ctx context.Context, shelf string) ([]bson.M, error) {
	cur, err := w.DB.Collection(shelf).Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(context.WithoutCancel(ctx))
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

// docTitle is the wiki page a doc came from: the wiki title if it has one,
// else the page behind its source URL, else its name.
func (w *Watcher) docTitle(doc bson.M) string {
	if t := strings.TrimSpace(toString(doc["wiki_title"])); t != "" {
		return t
	}
	if t := titleFromURL(toString(doc["source_url"])); t != "" {
		return t
	}
	return strings.TrimSpace(toString(doc["name"]))
}

// sourceURL builds the wiki URL for a page title.
func (w *Watcher) sourceURL(title string) string {
	return w.FeedURLBase() + "/wiki/" + wikifeed.PagePath(title)
}

// track registers a page in tracked_pages so the next poll can diff it.
func (w *Watcher) track(ctx context.Context, title, url, shelf string, rev int) error {
	_, err := w.DB.Collection("tracked_pages").UpdateOne(ctx,
		bson.M{"title": title},
		bson.M{"$set": bson.M{
			"url": url, "collection": shelf, "rev_id": rev,
		}},
		options.UpdateOne().SetUpsert(true))
	return err
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
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int32:
		return strconv.Itoa(int(t))
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return fmt.Sprint(v)
}
