// Package wiki is the helldivers.wiki.gg fallback: it shapes the model's
// question into a wiki lookup, then summarizes one page with a source
// link. Ports bot/wiki.py.
package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

// Stage names where a wiki lookup stopped.
type Stage string

const (
	// StageFound: the lookup resolved to a page that carries text.
	StageFound Stage = "found"
	// StageSearchMiss: the wiki answered, but no page matched any
	// shaped query — the wiki has nothing on the topic.
	StageSearchMiss Stage = "search-miss"
	// StageFetchFail: the wiki API failed (network, status, decode) —
	// the uplink is down, not the query.
	StageFetchFail Stage = "fetch-fail"
	// StageExtractEmpty: the page resolved but carries no extract text.
	StageExtractEmpty Stage = "empty-extract"
)

// Result is the outcome of one wiki lookup.
type Result struct {
	// Text is the summary handed to the model: "Title: extract…
	// [source: url]". Empty when the lookup found nothing.
	Text string
	// Title is the page that answered.
	Title string
	// Stage says where the lookup stopped.
	Stage Stage
	// Query is the shaped query that resolved it: the page title that
	// matched, or the search string that hit.
	Query string
	// Err is the underlying failure, if any.
	Err error
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

// lookupBudget caps one wiki lookup: the ladder makes several requests,
// and a fallback must not outlast the turn that asked for it.
const lookupBudget = 30 * time.Second

// maxRedirectHops bounds the redirect chain walked when matching a
// candidate title to its page.
const maxRedirectHops = 5

// Lookup resolves a natural-language query to one wiki page and returns
// its summary. The query is shaped server-side (see query.go), so the
// model can pass a plain question and still land on the right page.
//
// A miss says where it came from: StageSearchMiss (the wiki has nothing
// on the topic), StageFetchFail (the wiki API is unreachable) or
// StageExtractEmpty (the page resolved but carries no text). Ops read
// the stage from the logs; the caller reads it from the Result.
func (c *Client) Lookup(ctx context.Context, query string) Result {
	q := normalizeQuery(query)
	if q == "" {
		return Result{Stage: StageSearchMiss}
	}
	ctx, cancel := context.WithTimeout(ctx, lookupBudget)
	defer cancel()
	titles, searches := shape(q)

	stage := StageSearchMiss
	var cause error
	// note keeps the failure that explains most: a broken API explains
	// more than an empty page, which explains more than a query that
	// matched nothing.
	note := func(res Result) {
		if stageRank(res.Stage) > stageRank(stage) {
			stage, cause = res.Stage, res.Err
		}
	}

	// Exact page titles first: the wiki's own page index is the most
	// reliable "which page is this entity" signal, and it answers the
	// broad questions free-text search cannot — a search for "best
	// loadout vs Automatons" ranks mission pages above the faction
	// page, while the title index holds exactly one Automatons page.
	title, err := c.resolveTitle(ctx, titles)
	switch {
	case err != nil:
		// A failed title lookup is not fatal: the search path below
		// may still answer. The failure is recorded anyway — a lookup
		// that could not run says "we don't know", not "the wiki has
		// nothing".
		log.Printf("wiki: title lookup failed query=%q: %v", q, err)
		note(Result{Stage: StageFetchFail, Err: err})
	case title != "":
		res := c.summary(ctx, title)
		if res.Stage == StageFound {
			res.Query = title
			return res
		}
		log.Printf("wiki: %s title=%q query=%q", res.Stage, title, q)
		note(res)
	}

	for _, s := range searches {
		title, err := c.searchTitle(ctx, s)
		if err != nil {
			// The API is down: retrying the same calls will not help.
			log.Printf("wiki: search failed query=%q: %v", s, err)
			note(Result{Stage: StageFetchFail, Err: err})
			break
		}
		if title == "" {
			continue
		}
		res := c.summary(ctx, title)
		if res.Stage == StageFound {
			res.Query = s
			return res
		}
		log.Printf("wiki: %s title=%q query=%q", res.Stage, title, q)
		note(res)
	}

	log.Printf("wiki: %s query=%q", stage, q)
	return Result{Stage: stage, Query: q, Err: cause}
}

// stageRank orders failures by how much they explain.
func stageRank(s Stage) int {
	switch s {
	case StageFetchFail:
		return 3
	case StageExtractEmpty:
		return 2
	default:
		return 1
	}
}

// resolveTitle returns the first candidate that is an existing wiki
// page, resolved through MediaWiki redirects. One batched call covers
// every candidate.
func (c *Client) resolveTitle(ctx context.Context, candidates []string) (string, error) {
	if len(candidates) == 0 {
		return "", nil
	}
	resp, err := c.get(ctx, url.Values{
		"action": {"query"}, "titles": {strings.Join(candidates, "|")}, "redirects": {"1"},
	})
	if err != nil {
		return "", err
	}
	q, _ := resp["query"].(map[string]any)
	if q == nil {
		return "", nil
	}
	// The API answers with the canonical titles (and the redirect
	// targets), so the candidate that matched is found by title, not by
	// position.
	present := map[string]string{}
	pages, _ := q["pages"].([]any)
	for _, p := range pages {
		m, _ := p.(map[string]any)
		if m == nil {
			continue
		}
		if missing, _ := m["missing"].(bool); missing {
			continue
		}
		title, _ := m["title"].(string)
		if title != "" {
			present[strings.ToLower(title)] = title
		}
	}
	redirects := map[string]string{}
	list, _ := q["redirects"].([]any)
	for _, r := range list {
		m, _ := r.(map[string]any)
		if m == nil {
			continue
		}
		from, _ := m["from"].(string)
		to, _ := m["to"].(string)
		if from != "" && to != "" {
			redirects[strings.ToLower(from)] = to
		}
	}
	for _, cand := range candidates {
		key := strings.ToLower(cand)
		// Redirect chains resolve one hop per link, so a candidate two
		// hops from its page still has to be walked to the end.
		for hop := 0; hop < maxRedirectHops; hop++ {
			to, ok := redirects[key]
			if !ok {
				break
			}
			key = strings.ToLower(to)
		}
		if title, ok := present[key]; ok {
			return title, nil
		}
	}
	return "", nil
}

// searchTitle returns the top search hit for a query, or "" when the
// wiki has no match.
func (c *Client) searchTitle(ctx context.Context, query string) (string, error) {
	resp, err := c.get(ctx, url.Values{
		"action": {"query"}, "list": {"search"},
		"srsearch": {query}, "srlimit": {"1"}, "srnamespace": {"0"},
	})
	if err != nil {
		return "", err
	}
	return firstSearchTitle(resp), nil
}

// summary fetches one page's extract and formats it as
// "Title: extract… [source: url]".
func (c *Client) summary(ctx context.Context, title string) Result {
	f, err := c.get(ctx, url.Values{
		"action": {"query"}, "prop": {"extracts"}, "explaintext": {"1"},
		"titles": {title},
	})
	if err != nil {
		return Result{Title: title, Stage: StageFetchFail, Err: err}
	}
	text := firstExtract(f)
	if text == "" {
		return Result{Title: title, Stage: StageExtractEmpty}
	}
	text = spaces.ReplaceAllString(text, " ")
	text = cutRunes(text, 1200)
	return Result{
		Title: title,
		Stage: StageFound,
		Text: fmt.Sprintf("%s: %s [source: %s/wiki/%s]",
			title, text, c.api(), pagePath(title)),
	}
}

// pagePath builds the wiki path for a title. MediaWiki reads "/" in a
// title as a subpage separator, so the slashes stay and only the
// segments are escaped.
func pagePath(title string) string {
	parts := strings.Split(title, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(strings.ReplaceAll(p, " ", "_"))
	}
	return strings.Join(parts, "/")
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
