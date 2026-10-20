// Package mcpdb's server.go builds the MCP server over a ToolBackend. The
// production backend is *Executor (Mongo + live/wiki); tests substitute a
// stub, so no database is needed to verify the protocol plumbing.
package mcpdb

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nameArgs covers single-name tools (get_unit, get_gear, get_place,
// planet_status).
type nameArgs struct {
	Name string `json:"name" jsonschema:"exact name, e.g. Hulk Bruiser"`
}

// listUnitsArgs covers the faction roster: faction is required, role and
// limit narrow it, offset pages past the first page.
type listUnitsArgs struct {
	Faction string `json:"faction" jsonschema:"one of automatons, terminids, illuminate"`
	Role    string `json:"role,omitempty" jsonschema:"optional role substring filter, e.g. walker"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max docs to return (default 10, max 20)"`
	Offset  int    `json:"offset,omitempty" jsonschema:"roster offset for paging (default 0)"`
}

// listGearArgs covers the gear shelf: category is required, limit caps it,
// offset pages past the first page.
type listGearArgs struct {
	Category string `json:"category" jsonschema:"one of weapons, stratagems, armor, boosters"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max docs to return (default 10, max 20)"`
	Offset   int    `json:"offset,omitempty" jsonschema:"shelf offset for paging (default 0)"`
}

// wikiArgs covers the fuzzy fallback of last resort.
type wikiArgs struct {
	Query string `json:"query" jsonschema:"anything the database tools could not answer"`
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

// NewServer registers the eight intel tools over backend and returns a
// server ready to Run on any transport (stdio in production).
func NewServer(backend ToolBackend) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "rouge-mcpdb", Version: "1.0.0"}, nil)
	addGet := func(name, desc string) {
		mcp.AddTool(server, &mcp.Tool{Name: name, Description: desc},
			func(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
				return textResult(backend.Execute(ctx, name, toJSON(args)))
			})
	}
	addGet("get_unit", "Fetch one enemy unit or structure by exact name or alias (case-insensitive). Full doc: role, weak points, counters, source.")
	addGet("get_gear", "Fetch one weapon, stratagem, armor or booster by exact name or alias (case-insensitive).")
	addGet("get_place", "Fetch one static planet, biome, mission, difficulty or effect by exact name (case-insensitive).")
	mcp.AddTool(server, &mcp.Tool{Name: "list_units", Description: "List the faction-filtered unit roster, then get_unit for detail."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args listUnitsArgs) (*mcp.CallToolResult, any, error) {
			return textResult(backend.Execute(ctx, "list_units", toJSON(args)))
		})
	mcp.AddTool(server, &mcp.Tool{Name: "list_gear", Description: "List one gear shelf by category, then get_gear for detail."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args listGearArgs) (*mcp.CallToolResult, any, error) {
			return textResult(backend.Execute(ctx, "list_gear", toJSON(args)))
		})
	mcp.AddTool(server, &mcp.Tool{Name: "war_status", Description: "Live galactic war status: active fronts, hottest battles, divers deployed. No arguments."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return textResult(backend.Execute(ctx, "war_status", "{}"))
		})
	mcp.AddTool(server, &mcp.Tool{Name: "planet_status", Description: "Live war status for one planet: owner, liberation percent, active divers."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
			return textResult(backend.Execute(ctx, "planet_status", toJSON(args)))
		})
	mcp.AddTool(server, &mcp.Tool{Name: "wiki_search", Description: "Fall back to the Helldivers wiki when the database tools return nothing."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args wikiArgs) (*mcp.CallToolResult, any, error) {
			return textResult(backend.Execute(ctx, "wiki_search", toJSON(args)))
		})
	return server
}
