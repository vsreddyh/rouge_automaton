package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlanetLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Super-Client") == "" {
			t.Error("super-client header required")
		}
		w.Write([]byte(`[{"name":"Mintoria","currentOwner":"Automaton","health":380000,"maxHealth":1000000,"players":12400}]`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Contact: "t", Client: "t", HTTP: srv.Client()}
	got := c.PlanetLine(context.Background(), "mintoria")
	if !strings.Contains(got, "Mintoria: Automaton 62.0%") || !strings.HasSuffix(got, "Over.") {
		t.Fatalf("got %q", got)
	}
	if c.PlanetLine(context.Background(), "nope") != "" {
		t.Fatal("unknown planet must be empty")
	}
}

func TestPlanetLineDefenseEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"name":"FURY","currentOwner":"Humans","health":2000000,"maxHealth":2000000,` +
			`"players":null,"event":{"eventType":1,"faction":"Automaton",` +
			`"health":1336106,"maxHealth":1500000,"endTime":"2026-09-20T13:00:56Z"}}]`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Contact: "t", Client: "t", HTTP: srv.Client()}
	got := c.PlanetLine(context.Background(), "fury")
	for _, want := range []string{"FURY: Humans 0.0%", "DEFENSE vs Automaton", "10.9% repelled", "ends Sep 20", "Over."} {
		if !strings.Contains(got, want) {
			t.Fatalf("want %q in %q", want, got)
		}
	}
}

func TestCacheSurvivesOutage(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`[{"name":"Mintoria","currentOwner":"Humans","health":1000000,"maxHealth":1000000,"players":5}]`))
	}))
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	if got := c.PlanetLine(context.Background(), "mintoria"); got == "" {
		t.Fatal("first call must hit")
	}
	srv.Close() // outage: cache must serve instead of failing
	if got := c.PlanetLine(context.Background(), "mintoria"); !strings.Contains(got, "Mintoria: Humans") {
		t.Fatalf("cache must survive outage, got %q", got)
	}
}

func TestNon200Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	if got := c.PlanetLine(context.Background(), "mintoria"); got != "" {
		t.Fatalf("non-200 must not parse body: %q", got)
	}
}
