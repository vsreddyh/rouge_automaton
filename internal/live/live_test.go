package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixturePlanet is one roster planet for the canned war API.
type fixturePlanet struct {
	index   int
	name    string
	owner   int
	health  float64
	max     float64
	players float64
	// eventFaction non-empty adds a campaign entry for the planet.
	eventFaction string
	eventHealth  float64
	eventMax     float64
	defense      bool
	defenseRaw   any     // non-nil overrides defense, e.g. a numeric 1 shape
	expire       float64 // unix seconds, 0 = no expiry
	nameless     bool    // no name-table entry: unaddressable, must be skipped
}

// serveBodies serves the four Training Manual endpoints from canned bodies.
func serveBodies(t *testing.T, campaign, status, info, names string, code int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, p := range htmPaths {
		body := map[string]string{
			"/war/campaign": campaign, "/war/status": status,
			"/war/info": info, "/planets": names,
		}[p]
		b, c := body, code
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Super-Client") == "" {
				t.Error("super-client header required")
			}
			if c != 200 {
				w.WriteHeader(c)
				return
			}
			w.Write([]byte(b))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serveRoster serves the four Training Manual endpoints from planets,
// padding with quiet filler so fixtures clear the minRoster staleness floor.
func serveRoster(t *testing.T, planets []fixturePlanet) *httptest.Server {
	t.Helper()
	return servePlanets(t, planets, minRoster+20)
}

// servePlanets serves planets plus filler quiet planets until the roster
// holds total entries.
func servePlanets(t *testing.T, planets []fixturePlanet, total int) *httptest.Server {
	t.Helper()
	seen := map[int]bool{}
	for _, p := range planets {
		seen[p.index] = true
	}
	all := append([]fixturePlanet(nil), planets...)
	for i := 0; len(all) < total; i++ {
		idx := 1000 + i
		if seen[idx] {
			continue
		}
		all = append(all, fixturePlanet{
			index: idx, name: fmt.Sprintf("Filler %d", i),
			owner: 1, health: 1000000, max: 1000000,
		})
	}
	var campaign []map[string]any
	var status, infos []map[string]any
	names := map[string]any{}
	for _, p := range all {
		status = append(status, map[string]any{
			"index": p.index, "owner": p.owner, "health": p.health, "players": p.players,
		})
		infos = append(infos, map[string]any{"index": p.index, "maxHealth": p.max})
		if !p.nameless {
			names[fmt.Sprint(p.index)] = map[string]any{"name": p.name}
		}
		if p.eventFaction != "" {
			defense := any(p.defense)
			if p.defenseRaw != nil {
				defense = p.defenseRaw
			}
			camp := map[string]any{
				"planetIndex": p.index, "name": p.name, "faction": p.eventFaction,
				"players": p.players, "health": p.eventHealth, "maxHealth": p.eventMax,
				"defense": defense,
			}
			if p.expire > 0 {
				camp["expireDateTime"] = p.expire
			}
			campaign = append(campaign, camp)
		}
	}
	marshal := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	return serveBodies(t,
		marshal(campaign),
		marshal(map[string]any{"planetStatus": status}),
		marshal(map[string]any{"planetInfos": infos}),
		marshal(names), 200)
}

// newTestClient wires a Client at the canned server with the headers the
// fixture server requires.
func newTestClient(srv *httptest.Server) *Client {
	return &Client{Base: srv.URL, Contact: "t", Client: "t", HTTP: srv.Client()}
}

func TestPlanetLine(t *testing.T) {
	srv := serveRoster(t, []fixturePlanet{
		{index: 5, name: "Mintoria", owner: 3, health: 380000, max: 1000000, players: 12400},
	})
	c := newTestClient(srv)
	got := c.PlanetLine(context.Background(), "mintoria")
	if !strings.Contains(got, "Mintoria: Automatons 62.0%") || !strings.HasSuffix(got, "Over.") {
		t.Fatalf("got %q", got)
	}
	if c.PlanetLine(context.Background(), "nope") != "" {
		t.Fatal("unknown planet must be empty")
	}
}

func TestPlanetLineDefenseEvent(t *testing.T) {
	expire := float64(time.Date(2026, 9, 20, 13, 0, 56, 0, time.UTC).Unix())
	srv := serveRoster(t, []fixturePlanet{
		{index: 7, name: "FURY", owner: 1, health: 2000000, max: 2000000, players: 3100,
			eventFaction: "Automaton", eventHealth: 1336106, eventMax: 1500000,
			defense: true, expire: expire},
	})
	c := newTestClient(srv)
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
	expire := float64(time.Date(2026, 9, 20, 13, 0, 56, 0, time.UTC).Unix())
	srv := serveRoster(t, []fixturePlanet{
		{index: 7, name: "FURY", owner: 1, health: 2000000, max: 2000000, players: 3100,
			eventFaction: "Automaton", eventHealth: 1336106, eventMax: 1500000,
			defense: true, expire: expire},
		{index: 5, name: "Mintoria", owner: 3, health: 880000, max: 1000000, players: 900,
			eventFaction: "Automaton", eventHealth: 880000, eventMax: 1000000},
		{index: 9999, owner: 2, health: 100000, max: 1000000, players: 50, nameless: true},
		{index: 9, name: "Turing", owner: 1, health: 1000000, max: 1000000, players: 5},
	})
	c := newTestClient(srv)
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
	srv := serveRoster(t, []fixturePlanet{
		{index: 9, name: "Turing", owner: 1, health: 1000000, max: 1000000, players: 5},
	})
	c := newTestClient(srv)
	got := c.WarStatus(context.Background())
	if !strings.Contains(got, "no active fronts") || !strings.Contains(got, "5 divers deployed") {
		t.Fatalf("quiet war: %q", got)
	}
}

func TestWarStatusNoData(t *testing.T) {
	srv := serveBodies(t, "", "", "", "", 500)
	c := newTestClient(srv)
	if got := c.WarStatus(context.Background()); got != "" {
		t.Fatalf("no data must return empty, got %q", got)
	}
	if c.Up(context.Background()) {
		t.Fatal("no data must read as down")
	}
}

// TestStaleZeroPlayers pins the issue-46 guard: a full roster with every
// player count zeroed is a broken upstream snapshot, not a quiet war — the
// client must serve nothing so tools report the uplink down.
func TestStaleZeroPlayers(t *testing.T) {
	srv := serveRoster(t, []fixturePlanet{
		{index: 5, name: "Mintoria", owner: 3, health: 380000, max: 1000000},
		{index: 7, name: "FURY", owner: 1, health: 2000000, max: 2000000},
	})
	c := newTestClient(srv)
	if got := c.WarStatus(context.Background()); got != "" {
		t.Fatalf("zeroed roster must return empty, got %q", got)
	}
	if got := c.PlanetLine(context.Background(), "mintoria"); got != "" {
		t.Fatalf("zeroed roster must hide planets, got %q", got)
	}
	if c.Up(context.Background()) {
		t.Fatal("zeroed roster must read as down")
	}
}

// TestStaleSmallRoster pins the other half of the guard: a roster far below
// the full ~270-planet shape is implausibly small and must not print.
func TestStaleSmallRoster(t *testing.T) {
	srv := servePlanets(t, []fixturePlanet{
		{index: 5, name: "Mintoria", owner: 3, health: 380000, max: 1000000, players: 12400},
	}, 3)
	c := newTestClient(srv)
	if got := c.WarStatus(context.Background()); got != "" {
		t.Fatalf("tiny roster must return empty, got %q", got)
	}
}

func TestUp(t *testing.T) {
	srv := serveRoster(t, []fixturePlanet{
		{index: 5, name: "Mintoria", owner: 3, health: 380000, max: 1000000, players: 12400},
	})
	c := newTestClient(srv)
	if !c.Up(context.Background()) {
		t.Fatal("live roster must read as up")
	}
}

// TestEndTimeMillis pins parseEndTime against fractional-second timestamps:
// the API may emit millis, and the date must not silently drop.
func TestEndTimeMillis(t *testing.T) {
	expire := float64(time.Date(2026, 9, 20, 13, 0, 56, 123*1e6, time.UTC).UnixNano()) / 1e9
	srv := serveRoster(t, []fixturePlanet{
		{index: 7, name: "FURY", owner: 1, health: 2000000, max: 2000000, players: 10,
			eventFaction: "Automaton", eventHealth: 1336106, eventMax: 1500000,
			defense: true, expire: expire},
	})
	c := newTestClient(srv)
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
	srv := serveRoster(t, []fixturePlanet{
		{index: 5, name: "Mintoria", owner: 1, health: 1000000, max: 1000000, players: 5},
	})
	c := newTestClient(srv)
	if got := c.PlanetLine(context.Background(), "mintoria"); got == "" {
		t.Fatal("first call must hit")
	}
	srv.Close() // outage: cache must serve instead of failing
	// Backdate past the 5-minute TTL so the second call exercises the
	// refresh-fails-then-serve-cache path instead of short-circuiting.
	c.mu.Lock()
	c.at = time.Now().Add(-6 * time.Minute)
	c.mu.Unlock()
	if got := c.PlanetLine(context.Background(), "mintoria"); !strings.Contains(got, "Mintoria: Humans") {
		t.Fatalf("cache must survive outage, got %q", got)
	}
}

func TestNon200Empty(t *testing.T) {
	srv := serveBodies(t, "", "", "", "", 429)
	c := newTestClient(srv)
	if got := c.PlanetLine(context.Background(), "mintoria"); got != "" {
		t.Fatalf("non-200 must not parse body: %q", got)
	}
}

// TestNumericDefense pins isDefense against a non-bool shape: a 1 defense
// flag must still classify DEFENSE instead of a campaign.
func TestNumericDefense(t *testing.T) {
	expire := float64(time.Date(2026, 9, 20, 13, 0, 56, 0, time.UTC).Unix())
	srv := serveRoster(t, []fixturePlanet{
		{index: 7, name: "FURY", owner: 1, health: 2000000, max: 2000000, players: 3100,
			eventFaction: "Automaton", eventHealth: 1336106, eventMax: 1500000,
			defenseRaw: 1, expire: expire},
	})
	c := newTestClient(srv)
	if got := c.PlanetLine(context.Background(), "fury"); !strings.Contains(got, "DEFENSE vs Automaton") {
		t.Fatalf("numeric defense must classify DEFENSE: %q", got)
	}
}

// TestPlanetsArrayShape pins the name-table fallback: if the name endpoint
// ever ships an array instead of the string-keyed object, the roster still
// resolves instead of collapsing below the staleness floor.
func TestPlanetsArrayShape(t *testing.T) {
	const n = minRoster + 20
	status := make([]map[string]any, 0, n)
	infos := make([]map[string]any, 0, n)
	names := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		players := 0.0
		if i == 0 {
			players = 5
		}
		status = append(status, map[string]any{"index": i, "owner": 1, "health": 1000000, "players": players})
		infos = append(infos, map[string]any{"index": i, "maxHealth": 1000000})
		names = append(names, map[string]any{"name": fmt.Sprintf("Filler %d", i)})
	}
	marshal := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	srv := serveBodies(t, "[]",
		marshal(map[string]any{"planetStatus": status}),
		marshal(map[string]any{"planetInfos": infos}),
		marshal(names), 200)
	c := newTestClient(srv)
	if !c.Up(context.Background()) {
		t.Fatal("array name table must read as up")
	}
	if got := c.PlanetLine(context.Background(), "filler 7"); !strings.Contains(got, "Filler 7: Humans") {
		t.Fatalf("array name table must resolve: %q", got)
	}
}
