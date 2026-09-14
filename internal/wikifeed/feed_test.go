package wikifeed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
