// Package wikifeed reads the wiki RecentChanges Atom feed and the MediaWiki
// API. Ports the polling half of scripts/rss_watch.py.
package wikifeed

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBase is the Helldivers wiki. Tests override Client.Base.
const DefaultBase = "https://helldivers.wiki.gg"

// DefaultUA identifies the bot to wiki.gg (Content-Signal: use=reference).
const DefaultUA = "rouge-automaton-watcher (+admin@example.com)"

// Entry is one RecentChanges item.
type Entry struct {
	Title   string `xml:"title"`
	Updated string `xml:"updated"`
}

type atomFeed struct {
	Entries []Entry `xml:"entry"`
}

// ParseAtom decodes a MediaWiki Atom feed into entries.
func ParseAtom(r io.Reader) ([]Entry, error) {
	var f atomFeed
	if err := xml.NewDecoder(r).Decode(&f); err != nil {
		return nil, err
	}
	return f.Entries, nil
}

// Client talks to the MediaWiki API.
type Client struct {
	Base string
	HTTP *http.Client
}

// New returns a client against the live wiki.
func New() *Client { return &Client{Base: DefaultBase} }

func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	return DefaultBase
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return sharedHTTP
}

var sharedHTTP = &http.Client{Timeout: 20 * time.Second}

// FeedURL is the RecentChanges Atom endpoint with bot/categorization filters.
func (c *Client) FeedURL(days, limit int) string {
	q := url.Values{
		"action":             {"feedrecentchanges"},
		"feedformat":         {"atom"},
		"hidebots":           {"1"},
		"hidecategorization": {"1"},
		"urlversion":         {"2"},
		"days":               {fmt.Sprint(days)},
		"limit":              {fmt.Sprint(limit)},
	}
	return c.base() + "/api.php?" + q.Encode()
}

// Feed fetches and parses the RecentChanges feed. If since is non-empty it is
// sent as If-Modified-Since; a 304 returns (nil, lastModified, nil).
func (c *Client) Feed(ctx context.Context, days, limit int, since string) ([]Entry, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.FeedURL(days, limit), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", DefaultUA)
	if since != "" {
		req.Header.Set("If-Modified-Since", since)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	lm := resp.Header.Get("Last-Modified")
	if resp.StatusCode == http.StatusNotModified {
		return nil, lm, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, lm, fmt.Errorf("feed status %d", resp.StatusCode)
	}
	entries, err := ParseAtom(resp.Body)
	if err != nil {
		return nil, lm, err
	}
	return entries, lm, nil
}

func (c *Client) get(ctx context.Context, params url.Values) (map[string]any, error) {
	params.Set("format", "json")
	params.Set("formatversion", "2")
	u := c.base() + "/api.php?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", DefaultUA)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wiki status %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Page is the subset of a wiki page the watcher needs to refresh a doc.
type Page struct {
	Title     string
	RevID     int
	Timestamp string
	Wikitext  string
	Extract   string
}

// Page fetches one page's wikitext, plaintext extract, revision and timestamp.
// Returns Title (possibly redirected) and RevID 0 with nil error for a miss.
func (c *Client) Page(ctx context.Context, title string) (Page, error) {
	d, err := c.get(ctx, url.Values{
		"action":      {"query"},
		"prop":        {"revisions|extracts"},
		"rvprop":      {"ids|timestamp|content"},
		"rvslots":     {"main"},
		"explaintext": {"1"},
		"redirects":   {"1"},
		"titles":      {title},
	})
	if err != nil {
		return Page{}, err
	}
	return pageFrom(d), nil
}

// Revision returns the canonical title and current revision id. RevID is 0
// when the page does not exist.
func (c *Client) Revision(ctx context.Context, title string) (string, int, error) {
	d, err := c.get(ctx, url.Values{
		"action":    {"query"},
		"prop":      {"revisions"},
		"rvprop":    {"ids"},
		"redirects": {"1"},
		"titles":    {title},
	})
	if err != nil {
		return title, 0, err
	}
	p := pageFrom(d)
	return p.Title, p.RevID, nil
}

func pageFrom(d map[string]any) Page {
	var p Page
	query, _ := d["query"].(map[string]any)
	if query == nil {
		return p
	}
	pages, _ := query["pages"].([]any)
	if len(pages) == 0 {
		return p
	}
	pg, _ := pages[0].(map[string]any)
	if pg == nil {
		return p
	}
	if t, ok := pg["title"].(string); ok {
		p.Title = t
	}
	if boolVal(pg["missing"]) {
		return p
	}
	if ex, ok := pg["extract"].(string); ok {
		p.Extract = ex
	}
	revs, _ := pg["revisions"].([]any)
	if len(revs) == 0 {
		return p
	}
	rev, _ := revs[0].(map[string]any)
	if rev == nil {
		return p
	}
	p.RevID = int(numVal(rev["revid"]))
	if ts, ok := rev["timestamp"].(string); ok {
		p.Timestamp = ts
	}
	if slots, ok := rev["slots"].(map[string]any); ok {
		if main, ok := slots["main"].(map[string]any); ok {
			if content, ok := main["content"].(string); ok {
				p.Wikitext = content
			}
		}
	} else if content, ok := rev["content"].(string); ok {
		p.Wikitext = content
	}
	return p
}

func boolVal(v any) bool {
	b, _ := v.(bool)
	return b
}

func numVal(v any) float64 {
	f, _ := v.(float64)
	return f
}

// Slug converts a wiki title to its URL slug (spaces to underscores).
func Slug(title string) string {
	return strings.ReplaceAll(title, " ", "_")
}
