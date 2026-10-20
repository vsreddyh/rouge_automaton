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
	if len(tools.Tools) != 8 {
		t.Fatalf("want 8 tools, got %d", len(tools.Tools))
	}

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "get_unit",
		Arguments: map[string]any{"name": "hulk"},
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
	if text != "canned:get_unit" {
		t.Fatalf("content = %q", text)
	}
	if len(stub.calls) != 1 || stub.calls[0][0] != "get_unit" ||
		!strings.Contains(stub.calls[0][1], `"name":"hulk"`) {
		t.Fatalf("backend calls = %v", stub.calls)
	}
	// list_units must survive the protocol round trip with its typed args.
	lres, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "list_units",
		Arguments: map[string]any{"faction": "automatons"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lres.IsError {
		t.Fatal("list_units call flagged error")
	}
	ltext := ""
	for _, c := range lres.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			ltext += tc.Text
		}
	}
	if ltext != "canned:list_units" {
		t.Fatalf("list_units content = %q", ltext)
	}
	// war_status takes no arguments: the no-arg handler must survive the
	// protocol round trip (empty struct decode) and reach the backend.
	wres, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "war_status",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wres.IsError {
		t.Fatal("war_status call flagged error")
	}
	wtext := ""
	for _, c := range wres.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			wtext += tc.Text
		}
	}
	if wtext != "canned:war_status" {
		t.Fatalf("war_status content = %q", wtext)
	}
	if len(stub.calls) != 3 || stub.calls[2][0] != "war_status" {
		t.Fatalf("backend calls = %v", stub.calls)
	}
}
