// Package mcpdb's server.go builds the MCP server over a ToolBackend. The
// production backend is *Executor (Mongo + live/wiki); tests substitute a
// stub, so no database is needed to verify the protocol plumbing.
package mcpdb

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// searchArgs covers the three search tools: query is required, faction and
// k are optional (the executor defaults k to 3 and ignores bad factions).
type searchArgs struct {
	Query   string `json:"query" jsonschema:"keywords to search for"`
	Faction string `json:"faction,omitempty" jsonschema:"one of automatons, terminids, illuminate (search_units only)"`
	K       int    `json:"k,omitempty" jsonschema:"max docs to return (default 3, max 5)"`
}

// nameArgs covers single-name tools (planet_status).
type nameArgs struct {
	Name string `json:"name" jsonschema:"exact planet name, e.g. Mintoria"`
}

// toJSON re-encodes typed args so every tool funnels through the single
// Execute mapping — the MCP layer adds no logic of its own.
func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// textResult wraps plain-text tool output as MCP content.
func textResult(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

// NewServer registers the five intel tools over backend and returns a
// server ready to Run on any transport (stdio in production).
func NewServer(backend ToolBackend) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "rouge-mcpdb", Version: "1.0.0"}, nil)
	addSearch := func(name, desc string) {
		mcp.AddTool(server, &mcp.Tool{Name: name, Description: desc},
			func(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
				return textResult(backend.Execute(ctx, name, toJSON(args)))
			})
	}
	addSearch("search_units", "Search enemy units and structures (weak points, counters, health, anatomy). Use for any 'how to kill X' question.")
	addSearch("search_gear", "Search weapons, stratagems, armor and boosters (damage, cooldowns, costs). Use for loadout questions.")
	addSearch("search_world", "Search biomes, planets, missions, difficulties and effects. Use for where-to-fight questions.")
	mcp.AddTool(server, &mcp.Tool{Name: "planet_status", Description: "Live war status for one planet: owner, liberation percent, active divers."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
			return textResult(backend.Execute(ctx, "planet_status", toJSON(args)))
		})
	mcp.AddTool(server, &mcp.Tool{Name: "wiki_search", Description: "Fall back to the Helldivers wiki when the database tools return nothing."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
			return textResult(backend.Execute(ctx, "wiki_search", toJSON(args)))
		})
	return server
}
