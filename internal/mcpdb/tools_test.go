package mcpdb

import (
	"strings"
	"testing"
)

func TestToolDefs(t *testing.T) {
	defs := ToolDefs()
	if len(defs) != 6 {
		t.Fatalf("want 6 tools, got %d", len(defs))
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
	if got := e.Execute(t.Context(), "search_units", `not json`); !strings.Contains(got, "invalid arguments") {
		t.Fatalf("bad args: %q", got)
	}
	// Nil DB would panic on search; unknown-arg paths must not reach it.
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
