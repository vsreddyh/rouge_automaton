package mcpdb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rouge_automaton/internal/config"
	"rouge_automaton/internal/live"
)

func TestToolDefs(t *testing.T) {
	defs := ToolDefs()
	if len(defs) != 8 {
		t.Fatalf("want 8 tools, got %d", len(defs))
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if d.Name == "" || d.Description == "" {
			t.Fatalf("tool missing name/description: %+v", d)
		}
		if d.Parameters["type"] != "object" {
			t.Fatalf("tool %s parameters must be a JSON Schema object", d.Name)
		}
		if seen[d.Name] {
			t.Fatalf("duplicate tool %s", d.Name)
		}
		seen[d.Name] = true
	}
}

func TestExecuteUnknownAndBadArgs(t *testing.T) {
	e := &Executor{}
	if got := e.Execute(t.Context(), "frobnicate", `{}`); !strings.Contains(got, "unknown tool") {
		t.Fatalf("unknown tool: %q", got)
	}
	if got := e.Execute(t.Context(), "get_unit", `not json`); !strings.Contains(got, "invalid arguments") {
		t.Fatalf("bad args: %q", got)
	}
	// Nil DB must fail closed on exact tools, never panic.
	if got := e.Execute(t.Context(), "get_unit", `{"name":"hulk"}`); !strings.Contains(got, "not wired") {
		t.Fatalf("nil DB on get_unit: %q", got)
	}
	if got := e.Execute(t.Context(), "list_units", `{"faction":"automatons"}`); !strings.Contains(got, "not wired") {
		t.Fatalf("nil DB on list_units: %q", got)
	}
	if got := e.Execute(t.Context(), "get_gear", `{"name":"railcannon"}`); !strings.Contains(got, "not wired") {
		t.Fatalf("nil DB on get_gear: %q", got)
	}
	if got := e.Execute(t.Context(), "list_gear", `{"category":"weapons"}`); !strings.Contains(got, "not wired") {
		t.Fatalf("nil DB on list_gear: %q", got)
	}
	if got := e.Execute(t.Context(), "get_place", `{"name":"tundra"}`); !strings.Contains(got, "not wired") {
		t.Fatalf("nil DB on get_place: %q", got)
	}
	// Missing names and bad enums fail before touching the store.
	if got := e.Execute(t.Context(), "get_unit", `{}`); !strings.Contains(got, "needs a name") {
		t.Fatalf("empty get_unit: %q", got)
	}
	if got := e.Execute(t.Context(), "list_units", `{"faction":"bots"}`); !strings.Contains(got, "unknown faction") {
		t.Fatalf("bad faction: %q", got)
	}
	if got := e.Execute(t.Context(), "list_gear", `{"category":"gear"}`); !strings.Contains(got, "unknown category") {
		t.Fatalf("bad category: %q", got)
	}
	// Wiki and live guards fail closed without clients, never panic.
	if got := e.Execute(t.Context(), "wiki_search", `{}`); !strings.Contains(got, "needs a query") {
		t.Fatalf("empty wiki query: %q", got)
	}
	// war_status must fail closed without a live client, never panic.
	if got := e.Execute(t.Context(), "war_status", `{}`); !strings.Contains(got, "not wired") {
		t.Fatalf("nil live client: %q", got)
	}
	// Same guard must cover planet_status: PlanetLine derefs the client's
	// mutex, so an unwired Live would panic on &Executor{}.
	if got := e.Execute(t.Context(), "planet_status", `{"name":"Mintoria"}`); !strings.Contains(got, "not wired") {
		t.Fatalf("nil live client on planet_status: %q", got)
	}
	// Same for an unwired wiki client.
	if got := e.Execute(t.Context(), "wiki_search", `{"query":"hulk"}`); !strings.Contains(got, "not wired") {
		t.Fatalf("nil wiki client: %q", got)
	}
}

func TestIntArgShapes(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want int
	}{
		{float64(4), 4},
		{float32(3), 3},
		{2, 2},
		{int64(5), 5},
	} {
		if got := intArg(map[string]any{"k": tc.v}, "k", 3); got != tc.want {
			t.Fatalf("intArg(%v) = %d, want %d", tc.v, got, tc.want)
		}
	}
	if got := intArg(map[string]any{}, "k", 3); got != 3 {
		t.Fatalf("missing k must default, got %d", got)
	}
}

// liveServer serves a canned war roster (live=true) or a dead uplink
// (live=false) behind the four Training Manual endpoints.
func liveServer(t *testing.T, liveOK bool) *httptest.Server {
	t.Helper()
	bodies := map[string]string{}
	if liveOK {
		var status, infos []map[string]any
		names := map[string]any{}
		for i := 0; i < 120; i++ {
			idx := 1000 + i
			players := 0.0
			if i == 0 {
				players = 5
			}
			status = append(status, map[string]any{
				"index": idx, "owner": 1, "health": 1000000, "players": players,
			})
			infos = append(infos, map[string]any{"index": idx, "maxHealth": 1000000})
			names[fmt.Sprint(idx)] = map[string]any{"name": fmt.Sprintf("Filler %d", i)}
		}
		marshal := func(v any) string {
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
		bodies["/war/campaign"] = "[]"
		bodies["/war/status"] = marshal(map[string]any{"planetStatus": status})
		bodies["/war/info"] = marshal(map[string]any{"planetInfos": infos})
		bodies["/planets"] = marshal(names)
	}
	mux := http.NewServeMux()
	for _, p := range []string{"/war/campaign", "/war/status", "/war/info", "/planets"} {
		body := bodies[p]
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if !liveOK {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(body))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestPlanetStatusOutageVsUnknown pins the issue-46 contract: an unknown
// name on a live roster blames the spelling, but a dead uplink reports the
// uplink down instead of faulting the user's spelling.
func TestPlanetStatusOutageVsUnknown(t *testing.T) {
	e := &Executor{Live: live.New(config.Config{HD2APIBase: liveServer(t, true).URL})}
	if got := e.Execute(t.Context(), "planet_status", `{"name":"No Such Planet"}`); !strings.Contains(got, "check the spelling") {
		t.Fatalf("unknown name on live roster: %q", got)
	}
	e = &Executor{Live: live.New(config.Config{HD2APIBase: liveServer(t, false).URL})}
	if got := e.Execute(t.Context(), "planet_status", `{"name":"Mintoria"}`); !strings.Contains(got, "uplink as down") {
		t.Fatalf("outage must report uplink down: %q", got)
	}
}
