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

// TestWarStatus covers the generic "what's the war status" question: no
// planet name is supplied or needed, and the summary names active fronts,
// the faction tally (zero included), the busiest battle first, and total
// divers.
func TestWarStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[` +
			`{"name":"FURY","currentOwner":"Humans","health":2000000,"maxHealth":2000000,"players":3100,` +
			`"event":{"eventType":1,"faction":"Automaton","health":1336106,"maxHealth":1500000,"endTime":"2026-09-20T13:00:56Z"}},` +
			`{"name":"Mintoria","currentOwner":"Humans","health":880000,"maxHealth":1000000,"players":900,` +
			`"event":{"eventType":0,"faction":"Automaton","health":880000,"maxHealth":1000000}},` +
			`{"name":"Turing","currentOwner":"Humans","health":1000000,"maxHealth":1000000,"players":5}` +
			`]`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Contact: "t", Client: "t", HTTP: srv.Client()}
	got := c.WarStatus(context.Background())
	for _, want := range []string{
		"2 active fronts",
		"Automatons 2, Terminids 0, Illuminate 0",
		"FURY defense vs Automatons, 10.9% repelled, ends Sep 20, 3100 divers",
		"Mintoria campaign vs Automatons, 12.0% liberated, 900 divers",
		"4005 divers deployed",
		"Over.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("want %q in %q", want, got)
		}
	}
	// Busiest front leads the list.
	if i, j := strings.Index(got, "FURY"), strings.Index(got, "Mintoria"); i < 0 || j < 0 || i > j {
		t.Fatalf("busiest front must come first: %q", got)
	}
	// Non-event planets still count divers but are not fronts.
	if strings.Contains(got, "Turing") {
		t.Fatalf("quiet planet must not appear as a front: %q", got)
	}
}

func TestWarStatusQuiet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"name":"Turing","currentOwner":"Humans","health":1000000,"maxHealth":1000000,"players":5}]`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	got := c.WarStatus(context.Background())
	if !strings.Contains(got, "no active fronts") || !strings.Contains(got, "5 divers deployed") {
		t.Fatalf("quiet war: %q", got)
	}
}

func TestWarStatusNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	if got := c.WarStatus(context.Background()); got != "" {
		t.Fatalf("no data must return empty, got %q", got)
	}
}

// TestEndTimeMillis pins parseEndTime against fractional-second timestamps:
// the API may emit millis, and the date must not silently drop.
func TestEndTimeMillis(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"name":"FURY","currentOwner":"Humans","health":2000000,"maxHealth":2000000,` +
			`"players":10,"event":{"eventType":1,"faction":"Automaton",` +
			`"health":1336106,"maxHealth":1500000,"endTime":"2026-09-20T13:00:56.123Z"}}]`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Contact: "t", Client: "t", HTTP: srv.Client()}
	if got := c.PlanetLine(context.Background(), "fury"); !strings.Contains(got, "ends Sep 20") {
		t.Fatalf("millis endTime must keep the date: %q", got)
	}
	if got := c.WarStatus(context.Background()); !strings.Contains(got, "ends Sep 20") {
		t.Fatalf("millis endTime must keep the date in war status: %q", got)
	}
}

// TestNumIntShapes pins num() against non-float64 JSON shapes: an int
// eventType must still classify DEFENSE, and int health must count.
func TestNumIntShapes(t *testing.T) {
	m := map[string]any{"eventType": 1, "health": 380000, "maxHealth": 1000000}
	if num(m, "eventType") != 1 {
		t.Fatalf("int eventType must read 1, got %v", num(m, "eventType"))
	}
	if num(m, "health") != 380000 {
		t.Fatalf("int health must read 380000, got %v", num(m, "health"))
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
