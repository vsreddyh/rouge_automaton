// Package mcpdb exposes the intel store as model tools. It is the single
// implementation behind two fronts: the Discord bot's think-act loop (which
// calls Execute directly) and the standalone MCP server (which serves the
// same tools over stdio). One mapping, no duplication, no router.
package mcpdb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/vsreddyh/rouge_automaton/internal/config"
	"github.com/vsreddyh/rouge_automaton/internal/gorelay"
	"github.com/vsreddyh/rouge_automaton/internal/live"
	"github.com/vsreddyh/rouge_automaton/internal/wiki"
)

// ToolGuidance is appended to the system prompt so the model knows it must
// look things up instead of answering from memory. The persona voice itself
// stays in SOUL.md; this block is procedure, not character.
const ToolGuidance = `
Tool procedure: for any factual question (units, gear, places, war status),
call the matching search tool BEFORE answering — never answer from memory.
Never narrate intent ("Checking...", "Looking it up..."): either emit the
function call or the final answer — an ack without a call strands the user.
You may chain calls (e.g. search_units, then planet_status for the same
front). Every intel answer names weak point + counter + source, stays under
120 words, and ends with "Over." on its own beat. If a tool returns no
results, say what is unconfirmed and try wiki_search before giving up.`

// Executor runs tool calls against Mongo and the live/wiki backends.
type Executor struct {
	DB   *mongo.Database
	Live *live.Client
	Wiki *wiki.Client
}

// ToolBackend is the seam the MCP server (and tests) program against:
// anything that can execute a named tool call with a JSON argument object.
type ToolBackend interface {
	Execute(ctx context.Context, name, argsJSON string) string
}

// NewExecutor wires an executor from a database handle and the bot config
// (which carries the war-API credentials).
func NewExecutor(db *mongo.Database, cfg config.Config) *Executor {
	return &Executor{DB: db, Live: live.New(cfg), Wiki: wiki.New()}
}

// ToolDefs returns the function specs sent to the model on every turn.
// Keep this list short: each spec rides along in every request.
func ToolDefs() []gorelay.ToolDef {
	strProp := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	kProp := map[string]any{"type": "integer", "description": "max docs to return (default 3, max 5)"}
	return []gorelay.ToolDef{
		{Name: "search_units",
			Description: "Search enemy units and structures (weak points, counters, health, anatomy). Use for any 'how to kill X' question.",
			Parameters: obj(map[string]any{
				"query":   strProp("unit or structure name keywords, e.g. Hulk Bruiser"),
				"faction": map[string]any{"type": "string", "description": "one of automatons, terminids, illuminate", "enum": []string{"automatons", "terminids", "illuminate"}},
				"k":       kProp,
			}, "query")},
		{Name: "search_gear",
			Description: "Search weapons, stratagems, armor and boosters (damage, penetration, cooldowns, costs). Use for loadout questions.",
			Parameters: obj(map[string]any{
				"query": strProp("gear name or role keywords, e.g. Orbital Railcannon"),
				"k":     kProp,
			}, "query")},
		{Name: "search_world",
			Description: "Search biomes, planets, missions, difficulties and environmental effects (conditions, hazards, weather). Use for where-to-fight questions.",
			Parameters: obj(map[string]any{
				"query": strProp("place, biome, mission or effect keywords, e.g. Tundra"),
				"k":     kProp,
			}, "query")},
		{Name: "planet_status",
			Description: "Live war status for one planet: owner, liberation percent, active divers. Use for who-owns / status questions.",
			Parameters: obj(map[string]any{
				"name": strProp("exact planet name, e.g. Mintoria"),
			}, "name")},
		{Name: "wiki_search",
			Description: "Fall back to the Helldivers wiki when the database tools return nothing. Returns a summary with source link.",
			Parameters: obj(map[string]any{
				"query": strProp("anything the database tools could not answer"),
			}, "query")},
	}
}

// Execute runs one named tool call and returns plain-text results for the
// model. Unknown tools and bad arguments return explanatory text, never an
// error — the model recovers inside the loop instead of failing the turn.
func (e *Executor) Execute(ctx context.Context, name, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "invalid arguments: expected a JSON object"
	}
	query := strArg(args, "query")
	k := intArg(args, "k", 3)
	if k < 1 {
		k = 1
	}
	if k > 5 {
		k = 5
	}
	switch name {
	case "search_units":
		docs := SearchUnits(ctx, e.DB, query, strArg(args, "faction"), k)
		return withEmptyHint(FormatContext(docs), query)
	case "search_gear":
		docs := SearchGear(ctx, e.DB, query, k)
		return withEmptyHint(FormatContext(docs), query)
	case "search_world":
		docs := SearchWorld(ctx, e.DB, query, k)
		return withEmptyHint(FormatContext(docs), query)
	case "planet_status":
		if line := e.Live.PlanetLine(ctx, strArg(args, "name")); line != "" {
			return line
		}
		return "no live status for that planet — check the spelling or try wiki_search"
	case "wiki_search":
		if query == "" {
			return "wiki_search needs a query"
		}
		if res := e.Wiki.SearchFetch(ctx, query); res != "" {
			return res
		}
		return "wiki has nothing on that either — report it as unconfirmed"
	default:
		return fmt.Sprintf("unknown tool %q — available: search_units, search_gear, search_world, planet_status, wiki_search", name)
	}
}

// withEmptyHint turns an empty result into guidance the model can act on
// (try another tool) instead of a dead end it might present as fact.
func withEmptyHint(formatted, query string) string {
	if strings.TrimSpace(formatted) == "" {
		return "no local intel for that — try a broader query or wiki_search"
	}
	return formatted
}

func strArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

func intArg(args map[string]any, key string, def int) int {
	if f, ok := args[key].(float64); ok {
		return int(f)
	}
	return def
}
