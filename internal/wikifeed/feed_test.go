package wikifeed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseAtom(t *testing.T) {
	feed := `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry><title>Hulk Bruiser</title><updated>2026-09-14T00:00:00Z</updated></entry>
  <entry><title>Template:Nav</title><updated>2026-09-14T00:00:01Z</updated></entry>
</feed>`
	entries, err := ParseAtom(strings.NewReader(feed))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Title != "Hulk Bruiser" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestFeedNotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") == "" {
			t.Error("expected If-Modified-Since")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	entries, lm, err := c.Feed(context.Background(), 1, 10, "Mon, 14 Sep 2026 00:00:00 GMT")
	if err != nil || entries != nil {
		t.Fatalf("304 should be empty: %v %v lm=%q", entries, err, lm)
	}
}

func TestPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"query":{"pages":[{"title":"Hulk Bruiser","extract":"Big walker.","revisions":[{"revid":135127,"timestamp":"2026-09-14T00:00:00Z","slots":{"main":{"content":"{{Infobox Enemy|health=1,800}}"}}}]}]}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	p, err := c.Page(context.Background(), "Hulk_Bruiser")
	if err != nil {
		t.Fatal(err)
	}
	if p.RevID != 135127 || p.Extract != "Big walker." || !strings.Contains(p.Wikitext, "1,800") {
		t.Fatalf("page = %+v", p)
	}
}

func TestPageMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"query":{"pages":[{"title":"Nope","missing":true}]}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	p, err := c.Page(context.Background(), "Nope")
	if err != nil || p.RevID != 0 {
		t.Fatalf("missing page = %+v err=%v", p, err)
	}
}

func TestSlug(t *testing.T) {
	if Slug("Hulk Bruiser") != "Hulk_Bruiser" {
		t.Fatal("slug mismatch")
	}
}

// A rate-limited wiki must read as a rate limit: not an empty feed, not a
// decode error. The wiki's own reason has to survive into the error.
func TestFeedRateLimitPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"code":"ratelimited","info":"You've exceeded your rate limit. Please make sure you are setting a custom user agent with the username of your wiki account"}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	entries, _, err := c.Feed(context.Background(), 1, 10, "")
	if entries != nil {
		t.Fatalf("a refused feed is not an empty feed: %+v", entries)
	}
	if !IsRateLimit(err) {
		t.Fatalf("err = %v, want a rate limit", err)
	}
	if !strings.Contains(err.Error(), "ratelimited") {
		t.Fatalf("err = %v, want the wiki's reason in the message", err)
	}
}

// The edge answers 429 (and a bare 502 while its limiter is active) when a
// client is throttled. The poller must see a rate limit and back off.
func TestFeedTooManyRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	_, _, err := c.Feed(context.Background(), 1, 10, "")
	if !IsRateLimit(err) {
		t.Fatalf("err = %v, want a rate limit", err)
	}
	if got := RetryDelay(err, time.Minute); got != 2*time.Minute {
		t.Fatalf("retry delay = %s, want the wiki's 120s", got)
	}
}

// The client identifies itself: wiki.gg rate-limits clients it cannot
// identify, and the UA is the only way to name the wiki account.
func TestUserAgentIsSent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"></feed>`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client(), UA: "rouge-automaton-watcher (wiki account: tester; contact: t@example.com)"}
	if _, _, err := c.Feed(context.Background(), 1, 10, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "wiki account: tester") {
		t.Fatalf("UA = %q, want the wiki account named", got)
	}
}

// One batched revision lookup covers the whole feed: the wiki counts
// requests, and a poll must not spend one per entry.
func TestRevisionsBatched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"query":{"pages":[{"title":"Hulk Bruiser","revisions":[{"revid":5}]},{"title":"Trooper","revisions":[{"revid":7}]},{"title":"Gone","missing":true}]}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	revs, err := c.Revisions(context.Background(), []string{"Hulk Bruiser", "Trooper", "Gone"})
	if err != nil {
		t.Fatal(err)
	}
	if revs["Hulk Bruiser"].RevID != 5 || revs["Trooper"].RevID != 7 {
		t.Fatalf("revs = %+v", revs)
	}
	if revs["Gone"].RevID != 0 {
		t.Fatalf("a missing page must report rev 0: %+v", revs["Gone"])
	}
}

// The categories of a page are what routes it to a shelf.
func TestCategories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"query":{"pages":[{"title":"Hulk Bruiser","categories":[{"title":"Category:Enemies"},{"title":"Category:Automatons"},{"title":"Category:Large Enemies"}]}]}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	info, err := c.Categories(context.Background(), "Hulk Bruiser")
	if err != nil {
		t.Fatal(err)
	}
	if info.Missing || info.Title != "Hulk Bruiser" {
		t.Fatalf("info = %+v", info)
	}
	want := []string{"Enemies", "Automatons", "Large Enemies"}
	if len(info.Categories) != len(want) {
		t.Fatalf("categories = %v, want %v", info.Categories, want)
	}
	for i, c := range want {
		if info.Categories[i] != c {
			t.Fatalf("categories = %v, want %v", info.Categories, want)
		}
	}
}
