package mcpdb

import (
	"strings"
	"testing"
)

func TestToolDefs(t *testing.T) {
	defs := ToolDefs()
	if len(defs) != 5 {
		t.Fatalf("want 5 tools, got %d", len(defs))
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
}
