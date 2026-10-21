// Package mcpdb exposes the intel store as model tools. It is the single
// implementation behind two fronts: the Discord bot's think-act loop (which
// calls Execute directly) and the standalone MCP server (which serves the
// same tools over stdio). One mapping, no duplication, no router.
package mcpdb

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"rouge_automaton/internal/config"
	"rouge_automaton/internal/gorelay"
	"rouge_automaton/internal/live"
	"rouge_automaton/internal/wiki"
)

// ToolGuidance is appended to the system prompt so the model knows it must
// look things up instead of answering from memory. The persona voice itself
// stays in SOUL.md; this block is procedure, not character.
const ToolGuidance = `
Tool procedure: for any factual question (units, gear, places, war status),
fetch the exact doc by name first (get_unit, get_gear, get_place) — never
answer from memory. Aliases resolve server-side, so pass the name you heard.
When the name is unknown, list candidates first (list_units, list_gear),
then fetch the exact doc. War status ("war status", "how goes the war",
"what's the front") is one war_status call with no arguments, answered
directly — never stall on an ack.
Never narrate intent ("Checking...", "Looking it up...", "Pulling..."): if you
need data, your whole response is exactly one function call, never an
acknowledgement. Either emit the function call or the final answer — an ack
without a call strands the user with nothing.
You may chain calls (e.g. list_units, then get_unit; get_unit, then
planet_status for one planet). Every intel answer names weak point + counter
+ source, as fully as the question needs, and ends with "Over." on its own
beat. If a fetch misses, list-then-fetch before wiki_search; use wiki_search
only when the store has nothing. When the query is genuinely ambiguous and
no tool resolves it, ask one short clarifying question instead of guessing
or looping fetches until rounds run out.`

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
		if required == nil {
			// No-arg tools must marshal "required" as [] rather than null.
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	limitProp := map[string]any{"type": "integer", "description": "max docs to return (default 10, max 20)"}
	offsetProp := map[string]any{"type": "integer", "description": "roster offset for paging past the first page (default 0)"}
	return []gorelay.ToolDef{
		{Name: "get_unit",
			Description: "Fetch one enemy unit or structure by exact name or alias (case-insensitive, e.g. Hulk Bruiser or hulk). Full doc: role, weak points, counters, source. Use for any 'how to kill X' question.",
			Parameters: obj(map[string]any{
				"name": strProp("exact unit or structure name or alias, e.g. Hulk Bruiser"),
			}, "name")},
		{Name: "list_units",
			Description: "List the faction-filtered unit roster (names and roles). Use when the unit name is unknown or for 'what am I facing' questions, then get_unit for detail.",
			Parameters: obj(map[string]any{
				"faction": map[string]any{"type": "string", "description": "one of automatons, terminids, illuminate", "enum": []string{"automatons", "terminids", "illuminate"}},
				"role":    strProp("optional role substring filter, e.g. walker"),
				"limit":   limitProp,
				"offset":  offsetProp,
			}, "faction")},
		{Name: "get_gear",
			Description: "Fetch one weapon, stratagem, armor or booster by exact name or alias (case-insensitive). Full doc with source. Use for loadout detail questions.",
			Parameters: obj(map[string]any{
				"name": strProp("exact gear name or alias, e.g. Orbital Railcannon"),
			}, "name")},
		{Name: "list_gear",
			Description: "List one gear shelf by category (names only). Use when the gear name is unknown, then get_gear for detail.",
			Parameters: obj(map[string]any{
				"category": map[string]any{"type": "string", "description": "one of weapons, stratagems, armor, boosters", "enum": []string{"weapons", "stratagems", "armor", "boosters"}},
				"limit":    limitProp,
				"offset":   offsetProp,
			}, "category")},
		{Name: "get_place",
			Description: "Fetch one static planet, biome, mission, difficulty or effect by exact name (case-insensitive). Use for where-to-fight questions. Live ownership stays on planet_status.",
			Parameters: obj(map[string]any{
				"name": strProp("exact place, biome, mission or effect name, e.g. Tundra"),
			}, "name")},
		{Name: "war_status",
			Description: "Live galactic war status: active fronts per faction, hottest battles, divers deployed. Takes no arguments — use for any generic war-status question instead of asking the user for a planet.",
			Parameters:  obj(map[string]any{})},
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
	switch name {
	case "get_unit":
		want := strArg(args, "name")
		if want == "" {
			return "get_unit needs a name"
		}
		if e.DB == nil {
			return "local intel store not wired — report as unconfirmed"
		}
		if doc, _, ok := GetUnit(ctx, e.DB, want); ok {
			return FormatFull(doc)
		}
		return "no local intel for that name — try list_units for the canonical roster, or wiki_search"
	case "list_units":
		faction := strArg(args, "faction")
		if !Factions[strings.ToLower(faction)] {
			return "unknown faction — one of automatons, terminids, illuminate"
		}
		if e.DB == nil {
			return "local intel store not wired — report as unconfirmed"
		}
		docs := ListUnits(ctx, e.DB, faction, strArg(args, "role"), intArg(args, "limit", defaultListLimit), intArg(args, "offset", 0))
		return withEmptyHint(FormatContext(docs))
	case "get_gear":
		want := strArg(args, "name")
		if want == "" {
			return "get_gear needs a name"
		}
		if e.DB == nil {
			return "local intel store not wired — report as unconfirmed"
		}
		if doc, _, ok := GetGear(ctx, e.DB, want); ok {
			return FormatFull(doc)
		}
		return "no local intel for that name — try list_gear for the canonical shelf, or wiki_search"
	case "list_gear":
		category := strArg(args, "category")
		if !GearCategories[strings.ToLower(category)] {
			return "unknown category — one of weapons, stratagems, armor, boosters"
		}
		if e.DB == nil {
			return "local intel store not wired — report as unconfirmed"
		}
		docs := ListGear(ctx, e.DB, category, intArg(args, "limit", defaultListLimit), intArg(args, "offset", 0))
		return withEmptyHint(FormatContext(docs))
	case "get_place":
		want := strArg(args, "name")
		if want == "" {
			return "get_place needs a name"
		}
		if e.DB == nil {
			return "local intel store not wired — report as unconfirmed"
		}
		if doc, _, ok := GetPlace(ctx, e.DB, want); ok {
			return FormatFull(doc)
		}
		return "no local intel for that name — check the spelling or try wiki_search"
	case "planet_status":
		if strArg(args, "name") == "" {
			return "planet_status needs a name"
		}
		if e.Live == nil {
			return "live war API not wired — report the uplink as down, never guess"
		}
		if line, up := e.Live.PlanetLineUp(ctx, strArg(args, "name")); line != "" {
			return line
		} else if !up {
			return "live war API unreachable — report the uplink as down, never guess a war status"
		}
		return "no live status for that planet — check the spelling or try wiki_search"
	case "war_status":
		if e.Live == nil {
			return "live war API not wired — report the uplink as down, never guess"
		}
		if line := e.Live.WarStatus(ctx); line != "" {
			return line
		}
		return "live war API unreachable — report the uplink as down, never guess a war status"
	case "wiki_search":
		query := strArg(args, "query")
		if query == "" {
			return "wiki_search needs a query"
		}
		if e.Wiki == nil {
			return "wiki client not wired — report it as unconfirmed"
		}
		// The wiki shapes the query server-side, so a plain question
		// still lands on the right page. The stage separates "the wiki
		// has nothing" from "the wiki is down": the model must not
		// report a dead uplink as a missing page.
		res := e.Wiki.Lookup(ctx, query)
		switch res.Stage {
		case wiki.StageFound:
			return res.Text
		case wiki.StageFetchFail:
			return "wiki unreachable — report it as unconfirmed"
		case wiki.StageExtractEmpty:
			return "wiki page carries no text — report it as unconfirmed"
		default:
			return "wiki has nothing on that either — report it as unconfirmed"
		}
	default:
		return fmt.Sprintf("unknown tool %q — available: get_unit, list_units, get_gear, list_gear, get_place, war_status, planet_status, wiki_search", name)
	}
}

// withEmptyHint turns an empty result into guidance the model can act on
// (try another tool) instead of a dead end it might present as fact.
func withEmptyHint(formatted string) string {
	if strings.TrimSpace(formatted) == "" {
		return "no local intel for that — try a list tool for canonical names, or wiki_search"
	}
	return formatted
}

func strArg(args map[string]any, key string) string {
	return strings.TrimSpace(ToString(args[key]))
}

func intArg(args map[string]any, key string, def int) int {
	// Coerce through the same ToString gate the store uses, so every JSON
	// number shape (and numeric strings) resolves; clampLimit makes the
	// result safe and unparseable input falls back to the default.
	s := strings.TrimSpace(ToString(args[key]))
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64); err == nil {
		// int(f) is implementation-defined on NaN/Inf/out-of-range, and
		// the later clamp cannot save it — range-check against the
		// platform int first (1<<63 would still overflow a 32-bit int).
		if math.IsNaN(f) || math.IsInf(f, 0) || f >= float64(math.MaxInt) || f < float64(math.MinInt) {
			return def
		}
		return int(f)
	}
	return def
}
