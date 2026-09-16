package wiki

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearchFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list") == "search" {
			w.Write([]byte(`{"query":{"search":[{"title":"Hulk Bruiser"}]}}`))
			return
		}
		w.Write([]byte(`{"query":{"pages":[{"extract":"Big walker.  Rear vent."}]}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	got := c.SearchFetch(context.Background(), "hulk")
	if !strings.HasPrefix(got, "Hulk Bruiser:") || !strings.Contains(got, "[source:") {
		t.Fatalf("got %q", got)
	}
}

func TestEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"query":{"search":[]}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	if got := c.SearchFetch(context.Background(), "zzz"); got != "" {
		t.Fatalf("miss must be empty: %q", got)
	}
}

func TestMultibyteTruncate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list") == "search" {
			w.Write([]byte(`{"query":{"search":[{"title":"T—est"}]}}`))
			return
		}
		// 1500 em-dashes (3 bytes each): a byte-cut would split one.
		w.Write([]byte(`{"query":{"pages":[{"extract":"` + strings.Repeat("—", 1500) + `"}]}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	got := c.SearchFetch(context.Background(), "x")
	body := strings.SplitN(got, " [source:", 2)[0]
	// Title ("T—est: ") + extract cut to exactly 1200 runes = 1201 dashes.
	if strings.Count(body, "—") != 1201 {
		t.Fatalf("must cut extract at 1200 runes, got %d dashes", strings.Count(body, "—"))
	}
	if strings.ToValidUTF8(body, "") != body {
		t.Fatal("cut must not split multibyte runes")
	}
}

func TestSecondFetchFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list") == "search" {
			w.Write([]byte(`{"query":{"search":[{"title":"X"}]}}`))
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	if got := c.SearchFetch(context.Background(), "x"); got != "" {
		t.Fatalf("failed fetch must be empty: %q", got)
	}
}
