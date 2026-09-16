package mcpdb

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubBackend records calls and returns canned text: protocol plumbing is
// verified without a database.
type stubBackend struct {
	calls [][2]string
}

func (s *stubBackend) Execute(_ context.Context, name, argsJSON string) string {
	s.calls = append(s.calls, [2]string{name, argsJSON})
	return "canned:" + name
}

func TestServerToolsEndToEnd(t *testing.T) {
	stub := &stubBackend{}
	server := NewServer(stub)
	clientTrans, serverTrans := mcp.NewInMemoryTransports()

	srvDone := make(chan error, 1)
	go func() { srvDone <- server.Run(t.Context(), serverTrans) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), clientTrans, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 5 {
		t.Fatalf("want 5 tools, got %d", len(tools.Tools))
	}

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "search_units",
		Arguments: map[string]any{"query": "hulk", "faction": "automatons"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal("tool call flagged error")
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if text != "canned:search_units" {
		t.Fatalf("content = %q", text)
	}
	if len(stub.calls) != 1 || stub.calls[0][0] != "search_units" ||
		!strings.Contains(stub.calls[0][1], `"faction":"automatons"`) {
		t.Fatalf("backend calls = %v", stub.calls)
	}
}
