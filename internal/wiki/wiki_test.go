package wiki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeWiki stands in for helldivers.wiki.gg: it knows a set of pages and
// answers the three API calls the client makes (title lookup, search,
// extract).
type fakeWiki struct {
	// pages maps a canonical page title to its extract text.
	pages map[string]string
	// searches maps a search string to the top hit title.
	searches map[string]string
	// redirects maps a requested title to its redirect target.
	redirects map[string]string
	// status, when set, fails every call with that HTTP status.
	status int
}

// page finds the canonical title for a requested one, the way MediaWiki
// normalizes titles: case-insensitive, underscores for spaces.
func (f fakeWiki) page(title string) (string, bool) {
	want := normTitle(title)
	for name := range f.pages {
		if normTitle(name) == want {
			return name, true
		}
	}
	return "", false
}

// redirect resolves a requested title to its redirect target.
func (f fakeWiki) redirect(title string) (string, bool) {
	for from, to := range f.redirects {
		if normTitle(from) == normTitle(title) {
			return to, true
		}
	}
	return "", false
}

func (f fakeWiki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "rouge-automaton") {
		// MediaWiki asks every client to identify itself.
		http.Error(w, "bad user agent", 400)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("prop") == "extracts":
		title := q.Get("titles")
		canon, ok := f.page(title)
		if !ok {
			writeJSON(w, map[string]any{"query": map[string]any{"pages": []any{
				map[string]any{"title": title, "missing": true},
			}}})
			return
		}
		writeJSON(w, map[string]any{"query": map[string]any{"pages": []any{
			map[string]any{"title": canon, "extract": f.pages[canon]},
		}}})
	case q.Get("list") == "search":
		sr := q.Get("srsearch")
		title, ok := f.searches[sr]
		if !ok {
			writeJSON(w, map[string]any{"query": map[string]any{"search": []any{}}})
			return
		}
		writeJSON(w, map[string]any{"query": map[string]any{"search": []any{
			map[string]any{"title": title},
		}}})
	default:
		// The batched title lookup.
		pages := []any{}
		redirects := []any{}
		for _, cand := range strings.Split(q.Get("titles"), "|") {
			canon, found := f.page(cand)
			title := cand
			// Redirect chains: report every hop, the way the API does.
			for hop := 0; hop < 5 && !found; hop++ {
				to, isRedirect := f.redirect(title)
				if !isRedirect {
					break
				}
				redirects = append(redirects, map[string]any{"from": title, "to": to})
				title = to
				canon, found = f.page(title)
			}
			if !found {
				pages = append(pages, map[string]any{"title": cand, "missing": true})
				continue
			}
			pages = append(pages, map[string]any{"title": canon})
		}
		query := map[string]any{"pages": pages}
		if len(redirects) > 0 {
			query["redirects"] = redirects
		}
		writeJSON(w, map[string]any{"query": query})
	}
}

func writeJSON(w http.ResponseWriter, body map[string]any) {
	raw, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	_, _ = w.Write(raw)
}

// serve starts a fake wiki and returns a client pointed at it.
func serve(t *testing.T, f fakeWiki) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, HTTP: srv.Client()}
}

func TestLookupResolvesShapedQuery(t *testing.T) {
	// The convo-test query: raw natural language that scores zero hits
	// on the wiki. Shaping must land on the Automatons page.
	c := serve(t, fakeWiki{pages: map[string]string{
		"Automatons": "The Automatons are one of three factions the Helldivers can fight against.",
	}})
	res := c.Lookup(context.Background(), "best loadout vs Automatons Helldivers 2")
	if res.Stage != StageFound {
		t.Fatalf("stage = %q, want %q (err %v)", res.Stage, StageFound, res.Err)
	}
	if res.Title != "Automatons" {
		t.Fatalf("title = %q, want Automatons", res.Title)
	}
	if !strings.HasPrefix(res.Text, "Automatons:") || !strings.Contains(res.Text, "[source:") {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestLookupFallsBackToSearch(t *testing.T) {
	// No page title matches, so the search path answers.
	c := serve(t, fakeWiki{
		pages:    map[string]string{"Hulk Bruiser": "Big walker. Rear vent."},
		searches: map[string]string{"how do i kill hulk": "Hulk Bruiser"},
	})
	res := c.Lookup(context.Background(), "how do i kill hulk")
	if res.Stage != StageFound || res.Title != "Hulk Bruiser" {
		t.Fatalf("res = %+v", res)
	}
}

func TestLookupShortensQueryOnMiss(t *testing.T) {
	// The raw question and the de-framed keywords miss; a shorter run
	// of the keywords hits.
	c := serve(t, fakeWiki{
		pages:    map[string]string{"AR-23C Arc Cannon": "Anti-tank cannon."},
		searches: map[string]string{"cannon": "AR-23C Arc Cannon"},
	})
	res := c.Lookup(context.Background(), "what is the arc cannon")
	if res.Stage != StageFound || res.Title != "AR-23C Arc Cannon" {
		t.Fatalf("res = %+v", res)
	}
}

func TestLookupSearchMiss(t *testing.T) {
	c := serve(t, fakeWiki{})
	res := c.Lookup(context.Background(), "zzz")
	if res.Stage != StageSearchMiss {
		t.Fatalf("stage = %q, want %q", res.Stage, StageSearchMiss)
	}
	if res.Text != "" {
		t.Fatalf("a miss must carry no text: %q", res.Text)
	}
}

func TestLookupFetchFail(t *testing.T) {
	c := serve(t, fakeWiki{status: 500})
	res := c.Lookup(context.Background(), "hulk")
	if res.Stage != StageFetchFail {
		t.Fatalf("stage = %q, want %q", res.Stage, StageFetchFail)
	}
}

func TestLookupApiError(t *testing.T) {
	// MediaWiki answers 200 with an error object when it refuses the
	// request: that is a failed fetch, not an empty page.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"code":"badtitle","info":"Bad title."}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	res := c.Lookup(context.Background(), "hulk")
	if res.Stage != StageFetchFail {
		t.Fatalf("stage = %q, want %q", res.Stage, StageFetchFail)
	}
}

func TestLookupCapsQuery(t *testing.T) {
	// A pathological question must not build a multi-kilobyte URL.
	var mu sync.Mutex
	longest := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if n := len(r.URL.String()); n > longest {
			longest = n
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"query":{"search":[]}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	res := c.Lookup(context.Background(), strings.Repeat("automatons ", 200))
	if res.Stage != StageSearchMiss {
		t.Fatalf("stage = %q, want %q", res.Stage, StageSearchMiss)
	}
	mu.Lock()
	defer mu.Unlock()
	if longest > 2000 {
		t.Fatalf("request URL is %d chars: the query must be capped", longest)
	}
}

func TestCutQueryKeepsWords(t *testing.T) {
	// A capped question must not end in half a word.
	got := cutQuery(strings.Repeat("automatons ", 100), 50)
	if strings.HasSuffix(got, "automat") {
		t.Fatalf("cutQuery left a half word: %q", got)
	}
	if len([]rune(got)) > 50 {
		t.Fatalf("cutQuery = %d runes, want at most 50", len([]rune(got)))
	}
}

func TestLookupEmptyExtract(t *testing.T) {
	c := serve(t, fakeWiki{pages: map[string]string{"Tundra": ""}})
	res := c.Lookup(context.Background(), "Tundra")
	if res.Stage != StageExtractEmpty {
		t.Fatalf("stage = %q, want %q", res.Stage, StageExtractEmpty)
	}
}

func TestLookupFollowsRedirect(t *testing.T) {
	c := serve(t, fakeWiki{
		pages:     map[string]string{"Hulk (disambiguation)": "Hulks are large, heavily armored Automatons."},
		redirects: map[string]string{"hulk": "Hulk (disambiguation)"},
	})
	res := c.Lookup(context.Background(), "hulk")
	if res.Stage != StageFound || res.Title != "Hulk (disambiguation)" {
		t.Fatalf("res = %+v", res)
	}
}

func TestLookupFollowsRedirectChain(t *testing.T) {
	// A candidate two hops from its page still has to resolve.
	c := serve(t, fakeWiki{
		pages: map[string]string{"Hulk Bruiser": "Big walker. Rear vent."},
		redirects: map[string]string{
			"hulk":        "the bruiser",
			"the bruiser": "Hulk Bruiser",
		},
	})
	res := c.Lookup(context.Background(), "hulk")
	if res.Stage != StageFound || res.Title != "Hulk Bruiser" {
		t.Fatalf("res = %+v", res)
	}
}

func TestPagePathKeepsSubpages(t *testing.T) {
	// MediaWiki reads "/" as a subpage separator, so the slashes stay.
	if got := pagePath("Blitz Search And Destroy/Automaton"); got != "Blitz_Search_And_Destroy/Automaton" {
		t.Fatalf("pagePath = %q", got)
	}
}

func TestLookupMatchesUnderscoredTitle(t *testing.T) {
	// MediaWiki matches a title whatever it spells its spaces with.
	c := serve(t, fakeWiki{pages: map[string]string{
		"Blitz_Search_And_Destroy/Automaton": "Automaton mission.",
	}})
	res := c.Lookup(context.Background(), "Blitz Search And Destroy/Automaton")
	if res.Stage != StageFound || res.Title != "Blitz_Search_And_Destroy/Automaton" {
		t.Fatalf("res = %+v", res)
	}
}

func TestLookupEmptyQuery(t *testing.T) {
	c := serve(t, fakeWiki{})
	if res := c.Lookup(context.Background(), "   "); res.Stage != StageSearchMiss {
		t.Fatalf("stage = %q, want %q", res.Stage, StageSearchMiss)
	}
}

func TestLookupSkipsFramingOnlyQuery(t *testing.T) {
	// A query with no topic must not reach the wiki at all.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected for a framing-only query: %s", r.URL)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	res := c.Lookup(context.Background(), "what is it")
	if res.Stage != StageSearchMiss {
		t.Fatalf("stage = %q, want %q", res.Stage, StageSearchMiss)
	}
}

func TestMultibyteTruncate(t *testing.T) {
	c := serve(t, fakeWiki{pages: map[string]string{"T—est": strings.Repeat("—", 1500)}})
	res := c.Lookup(context.Background(), "T—est")
	body := strings.SplitN(res.Text, " [source:", 2)[0]
	// Title ("T—est: ") + extract cut to exactly 1200 runes = 1201 dashes.
	if strings.Count(body, "—") != 1201 {
		t.Fatalf("must cut extract at 1200 runes, got %d dashes", strings.Count(body, "—"))
	}
	if strings.ToValidUTF8(body, "") != body {
		t.Fatal("cut must not split multibyte runes")
	}
}
