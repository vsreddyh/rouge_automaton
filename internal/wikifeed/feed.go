// Package wikifeed reads the wiki RecentChanges Atom feed and the MediaWiki
// API. Ports the polling half of scripts/rss_watch.py.
package wikifeed

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBase is the Helldivers wiki. Tests override Client.Base.
const DefaultBase = "https://helldivers.wiki.gg"

// DefaultUA identifies the bot to wiki.gg (Content-Signal: use=reference).
// The real value comes from config.WikiUserAgent; this is the fallback used
// by direct callers and tests, and it still names the wiki account and a
// reachable contact because wiki.gg rate-limits anonymous clients.
const DefaultUA = "rouge-automaton-watcher (wiki account: vsreddyh; contact: shouryanreddyh@gmail.com; use=reference)"

// maxFeedBytes bounds one feed/error body: a rate-limit page or a broken
// proxy answer must not balloon the watcher.
const maxFeedBytes = 4 << 20

// RateLimitError is wiki.gg refusing a client for going too fast. It is a
// distinct error, not an empty feed and not a decode failure, so the poller
// can back off and surface the wiki's reason in the logs.
type RateLimitError struct {
	Status     int
	Code       string
	Info       string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	msg := "wiki rate limit"
	if e.Status > 0 {
		msg += fmt.Sprintf(" (status %d)", e.Status)
	}
	if e.Code != "" {
		msg += ": " + e.Code
	}
	if e.Info != "" {
		msg += ": " + e.Info
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	return msg
}

// IsRateLimit reports whether err is wiki.gg refusing this client for rate
// limiting. Callers use it to choose a longer backoff than the poll interval.
func IsRateLimit(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}

// RetryDelay returns the wiki's Retry-After when it gave one, otherwise def.
func RetryDelay(err error, def time.Duration) time.Duration {
	var rl *RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return rl.RetryAfter
	}
	return def
}

// Entry is one RecentChanges item. Published carries the creation time the
// feed reports (the same value as Updated for a brand-new page), so a
// discovery path can tell a creation from a later edit if it needs to.
type Entry struct {
	Title     string `xml:"title"`
	Updated   string `xml:"updated"`
	Published string `xml:"published"`
	ID        string `xml:"id"`
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
	// UA identifies the client. Set from config.WikiUserAgent in production;
	// DefaultUA keeps direct callers identifiable.
	UA string
}

// New returns a client against the live wiki.
func New() *Client { return &Client{Base: DefaultBase, UA: DefaultUA} }

// NewWithUA returns a client with an operator-supplied User-Agent. An empty
// value falls back to DefaultUA so misconfiguration never sends a blank UA.
func NewWithUA(ua string) *Client {
	c := New()
	if strings.TrimSpace(ua) != "" {
		c.UA = ua
	}
	return c
}

func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	return DefaultBase
}

func (c *Client) userAgent() string {
	if c.UA != "" {
		return c.UA
	}
	return DefaultUA
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
	req.Header.Set("User-Agent", c.userAgent())
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return nil, lm, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, lm, responseError(resp, body)
	}
	// wiki.gg's limiter sometimes answers 200 with a JSON error object; read
	// that as a rate limit (or the wiki's error), never as an empty Atom feed.
	if code, info, ok := wikiError(body); ok {
		return nil, lm, wikiErrorFor(resp, code, info)
	}
	entries, err := ParseAtom(bytes.NewReader(body))
	if err != nil {
		return nil, lm, err
	}
	return entries, lm, nil
}

// responseError classifies a non-200 wiki answer. A 429 (and the edge's 502
// while its limiter is active) becomes RateLimitError so the poller backs off;
// any other status keeps the old "wiki status N" contract.
func responseError(resp *http.Response, body []byte) error {
	code, info, _ := wikiError(body)
	if code == "" && len(body) > 0 {
		// A plain-text proxy error still carries the reason sometimes
		// ("You've exceeded your rate limit"); keep a short snippet.
		info = strings.TrimSpace(string(body))
		if len(info) > 200 {
			info = info[:200]
		}
	}
	lower := strings.ToLower(code + " " + info)
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		strings.Contains(lower, "ratelimit") ||
		strings.Contains(lower, "rate limit")
	if limited {
		return &RateLimitError{
			Status:     resp.StatusCode,
			Code:       code,
			Info:       info,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if code != "" || info != "" {
		return fmt.Errorf("wiki status %d: %s", resp.StatusCode, strings.TrimSpace(code+": "+info))
	}
	return fmt.Errorf("wiki status %d", resp.StatusCode)
}

// wikiError reads the {"error":{"code":…,"info":…}} payload MediaWiki uses
// both for refusals and for some 200 responses.
func wikiError(body []byte) (code, info string, ok bool) {
	var out struct {
		Error struct {
			Code string `json:"code"`
			Info string `json:"info"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Error.Code == "" {
		return "", "", false
	}
	return out.Error.Code, out.Error.Info, true
}

// wikiErrorFor turns a bare code/info pair into the right error type.
func wikiErrorFor(resp *http.Response, code, info string) error {
	lower := strings.ToLower(code + " " + info)
	if strings.Contains(lower, "ratelimit") || strings.Contains(lower, "rate limit") ||
		resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{
			Status:     resp.StatusCode,
			Code:       code,
			Info:       info,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	return fmt.Errorf("wiki api error: %s: %s", code, info)
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func (c *Client) get(ctx context.Context, params url.Values) (map[string]any, error) {
	params.Set("format", "json")
	params.Set("formatversion", "2")
	u := c.base() + "/api.php?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	// MediaWiki answers 200 with an error object when it refuses the
	// request; that is a failed fetch, not an empty page.
	if apiErr, ok := out["error"]; ok {
		m, _ := apiErr.(map[string]any)
		code, _ := m["code"].(string)
		info, _ := m["info"].(string)
		return nil, wikiErrorFor(resp, code, info)
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

// Revision is one page's current revision.
type Revision struct {
	Title string
	RevID int
}

// Revisions looks up several pages in one request: the poller would
// otherwise spend one wiki request per feed entry, and the wiki's limiter
// counts requests. The result is keyed by the requested title.
func (c *Client) Revisions(ctx context.Context, titles []string) (map[string]Revision, error) {
	out := map[string]Revision{}
	if len(titles) == 0 {
		return out, nil
	}
	d, err := c.get(ctx, url.Values{
		"action":    {"query"},
		"prop":      {"revisions"},
		"rvprop":    {"ids"},
		"redirects": {"1"},
		"titles":    {strings.Join(titles, "|")},
	})
	if err != nil {
		return nil, err
	}
	query, _ := d["query"].(map[string]any)
	if query == nil {
		return out, nil
	}
	// The API answers with the canonical titles, plus the redirect and
	// normalization tables that say which title became which.
	pages, _ := query["pages"].([]any)
	byTitle := map[string]Revision{}
	for _, p := range pages {
		m, _ := p.(map[string]any)
		if m == nil {
			continue
		}
		title, _ := m["title"].(string)
		if title == "" {
			continue
		}
		rev := Revision{Title: title}
		if !boolVal(m["missing"]) {
			revs, _ := m["revisions"].([]any)
			if len(revs) > 0 {
				rm, _ := revs[0].(map[string]any)
				if rm != nil {
					rev.RevID = int(numVal(rm["revid"]))
				}
			}
		}
		byTitle[title] = rev
	}
	links := map[string]string{}
	for _, key := range []string{"redirects", "normalized"} {
		list, _ := query[key].([]any)
		for _, item := range list {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			from, _ := m["from"].(string)
			to, _ := m["to"].(string)
			if from != "" && to != "" {
				links[from] = to
			}
		}
	}
	for _, want := range titles {
		key := want
		for hop := 0; hop < maxRedirectHops; hop++ {
			to, ok := links[key]
			if !ok {
				break
			}
			key = to
		}
		rev, ok := byTitle[key]
		if !ok {
			// A title the API did not report is a page that does not exist.
			out[want] = Revision{Title: want}
			continue
		}
		out[want] = rev
	}
	return out, nil
}

// maxRedirectHops bounds the redirect chain walked when resolving a batched
// title to its page.
const maxRedirectHops = 5

// Categories returns a page's categories and canonical title. Missing is
// true when the page does not exist. The category names are stripped of the
// "Category:" prefix so routing compares the wiki's own labels ("Enemies",
// "Armor", "Primary Weapons") directly.
func (c *Client) Categories(ctx context.Context, title string) (PageInfo, error) {
	d, err := c.get(ctx, url.Values{
		"action":    {"query"},
		"prop":      {"categories"},
		"cllimit":   {"max"},
		"redirects": {"1"},
		"titles":    {title},
	})
	if err != nil {
		return PageInfo{}, err
	}
	return categoriesFrom(d), nil
}

// PageInfo is the routing subset of a wiki page.
type PageInfo struct {
	Title      string
	Missing    bool
	Categories []string
}

func categoriesFrom(d map[string]any) PageInfo {
	var p PageInfo
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
		p.Missing = true
		return p
	}
	list, _ := pg["categories"].([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		cat, _ := m["title"].(string)
		cat = strings.TrimPrefix(cat, "Category:")
		if cat != "" {
			p.Categories = append(p.Categories, cat)
		}
	}
	return p
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

// PagePath builds the wiki path for a title. MediaWiki reads "/" in a
// title as a subpage separator, so the slashes stay and only the
// segments are escaped: a title carrying "?" or "#" must not produce a
// broken link.
func PagePath(title string) string {
	parts := strings.Split(title, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(Slug(p))
	}
	return strings.Join(parts, "/")
}
