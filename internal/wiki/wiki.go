// Package wiki is the helldivers.wiki.gg fallback: 1 search + 1 fetch,
// summarized with a source link. Ports bot/wiki.py.
package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var spaces = regexp.MustCompile(`\s+`)

// cutRunes truncates to n runes (multibyte-safe).
func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Client fetches wiki extracts. Base defaults to the live wiki; tests override.
type Client struct {
	Base string
	HTTP *http.Client
}

// New returns a Client targeting the live wiki.
func New() *Client { return &Client{} }

func (c *Client) api() string {
	if c.Base != "" {
		return c.Base
	}
	return "https://helldivers.wiki.gg"
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return sharedHTTP
}

// sharedHTTP is the process-wide client (keep-alive, single timeout).
var sharedHTTP = &http.Client{Timeout: 20 * time.Second}

func (c *Client) get(ctx context.Context, params url.Values) (map[string]any, error) {
	params.Set("format", "json")
	params.Set("formatversion", "2")
	u := c.api() + "/api.php?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "rouge-automaton (+admin@example.com)")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("wiki status %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// SearchFetch returns "Title: extract… [source: url]" or "" on any failure.
func (c *Client) SearchFetch(ctx context.Context, query string) string {
	s, err := c.get(ctx, url.Values{
		"action": {"query"}, "list": {"search"},
		"srsearch": {query}, "srlimit": {"1"}, "srnamespace": {"0"},
	})
	if err != nil {
		return ""
	}
	title := firstSearchTitle(s)
	if title == "" {
		return ""
	}
	f, err := c.get(ctx, url.Values{
		"action": {"query"}, "prop": {"extracts"}, "explaintext": {"1"},
		"titles": {title},
	})
	if err != nil {
		return ""
	}
	text := firstExtract(f)
	if text == "" {
		return ""
	}
	text = spaces.ReplaceAllString(text, " ")
	text = cutRunes(text, 1200)
	return fmt.Sprintf("%s: %s [source: %s/wiki/%s]",
		title, text, c.api(), strings.ReplaceAll(title, " ", "_"))
}

func firstSearchTitle(s map[string]any) string {
	q, _ := s["query"].(map[string]any)
	if q == nil {
		return ""
	}
	hits, _ := q["search"].([]any)
	if len(hits) == 0 {
		return ""
	}
	t, _ := hits[0].(map[string]any)["title"].(string)
	return t
}

func firstExtract(f map[string]any) string {
	q, _ := f["query"].(map[string]any)
	if q == nil {
		return ""
	}
	pages, _ := q["pages"].([]any)
	if len(pages) == 0 {
		return ""
	}
	t, _ := pages[0].(map[string]any)["extract"].(string)
	return t
}
