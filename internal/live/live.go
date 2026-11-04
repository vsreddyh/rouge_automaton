// Package live serves Type-L galactic war status from the Helldivers
// Training Manual community API (a live mirror of the official war feed)
// with a 5-minute cache. Ports bot/live.py cache semantics.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rouge_automaton/internal/config"
)

// Client queries the war API. Mirrors bot/live.py cache semantics.
type Client struct {
	Base    string
	Contact string
	Client  string
	HTTP    *http.Client

	mu sync.RWMutex
	at time.Time
	// retry is the earliest a failed refresh may be retried: without it
	// every call during an outage refires all five endpoints.
	retry time.Time
	plans []map[string]any
	// mo caches the standing assignments (Major Order plus
	// strategic-threat tasks) from the same refresh; names resolves
	// planet-index objectives against the shared index table.
	mo    []map[string]any
	names map[string]string
}

// New builds a Client from the bot config.
func New(cfg config.Config) *Client {
	return &Client{Base: cfg.HD2APIBase, Contact: cfg.XSuperContact, Client: cfg.XSuperClient}
}
func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return sharedHTTP
}

// sharedHTTP is the process-wide client (keep-alive, single timeout).
var sharedHTTP = &http.Client{Timeout: 20 * time.Second}

// planets returns the cached roster, refreshing it past the 5-minute TTL.
// The slice is shared and read-only: callers must not mutate it. A failed
// refresh serves the cache (or nothing) and backs off 30 seconds so an
// outage does not refire all five endpoints on every call.
func (c *Client) planets(ctx context.Context) []map[string]any {
	return c.snapshot(ctx).plans
}

// snapshot returns the cached feed read, refreshing it past the 5-minute
// TTL. One call, one epoch: liveness and assignments always come from the
// same read, so concurrent refreshes can never mix them.
func (c *Client) snapshot(ctx context.Context) snapshot {
	c.mu.RLock()
	plans, mo, names, at, retry := c.plans, c.mo, c.names, c.at, c.retry
	c.mu.RUnlock()
	if time.Since(at) >= 5*time.Minute || plans == nil {
		if time.Now().Before(retry) {
			return snapshot{plans: plans, mo: mo, names: names}
		}
		if fresh := c.refresh(ctx); fresh != nil {
			c.mu.Lock()
			c.plans, c.mo, c.names, c.at, c.retry = fresh.plans, fresh.mo, fresh.names, time.Now(), time.Time{}
			c.mu.Unlock()
			return *fresh
		}
		c.mu.Lock()
		if ctx.Err() == nil {
			c.retry = time.Now().Add(30 * time.Second)
		}
		c.mu.Unlock()
	}
	return snapshot{plans: plans, mo: mo, names: names}
}

// snapshot is one coherent feed read: the planet roster, the standing
// assignments, and the index-to-name table the planet objectives resolve
// against. refresh builds all three or nothing (see htmPaths).
type snapshot struct {
	plans []map[string]any
	mo    []map[string]any
	names map[string]string
}

// htmPaths are the Training Manual endpoints joined into one roster per
// refresh, all rooted at Base: campaign carries the active-front detail
// (faction, defense, expiry), status the galaxy-wide per-planet owners and
// player counts, info the per-planet max health, planets the
// index-to-name table, and major-orders the standing Major Order plus
// strategic-threat tasks. One feed family, one lifecycle: any fetch or
// decode failure returns nil so the caller keeps serving the cache (or
// reports the uplink down when there is no cache at all).
var htmPaths = []string{"/war/campaign", "/war/status", "/war/info", "/planets", "/war/major-orders"}

// minRoster is the staleness floor for a full-roster refresh: the live
// roster runs ~270 planets, so anything far smaller is a broken upstream
// snapshot, not a quiet war — treat it as an outage and keep the cache.
const minRoster = 100

// fetchBody GETs url with the war-API headers, returning the body on 200.
func fetchBody(ctx context.Context, client *http.Client, url, superClient, contact string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Super-Client", superClient)
	req.Header.Set("X-Super-Contact", contact)
	req.Header.Set("User-Agent", "rouge-automaton/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("war api %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// refresh rebuilds the roster from the war API. Any fetch or decode failure
// — or a stale-looking payload (implausibly small roster, zero divers
// galaxy-wide, ragged assignments) — returns nil so the caller keeps
// serving the cache, or reports the uplink down when there is no cache
// at all.
func (c *Client) refresh(ctx context.Context) *snapshot {
	bodies := make([][]byte, len(htmPaths))
	errs := make([]error, len(htmPaths))
	// Trim here (not in New) so direct &Client{Base: ...} literals,
	// as in tests, get the same normalization, however many slashes.
	base := strings.TrimRight(c.Base, "/")
	var wg sync.WaitGroup
	for i, p := range htmPaths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			body, err := fetchBody(ctx, c.http(), base+p, c.Client, c.Contact)
			bodies[i], errs[i] = body, err
		}(i, p)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil
		}
	}
	var campaign []map[string]any
	var status, info map[string]any
	var namesRaw any
	var assignments []map[string]any
	if err := json.Unmarshal(bodies[0], &campaign); err != nil {
		return nil
	}
	if err := json.Unmarshal(bodies[1], &status); err != nil {
		return nil
	}
	if err := json.Unmarshal(bodies[2], &info); err != nil {
		return nil
	}
	if err := json.Unmarshal(bodies[3], &namesRaw); err != nil {
		return nil
	}
	if err := json.Unmarshal(bodies[4], &assignments); err != nil {
		return nil
	}
	// Every assignment must carry its setting block: a ragged feed is a
	// broken snapshot, not a quiet front — fail like any other endpoint.
	// An empty array is valid (no standing order) and flows through.
	for _, a := range assignments {
		if _, ok := a["setting"].(map[string]any); !ok {
			return nil
		}
	}
	names := nameTable(namesRaw)
	if plans := joinRoster(campaign, status, info, names); plans != nil {
		return &snapshot{plans: plans, mo: assignments, names: names}
	}
	return nil
}

// nameTable normalizes the planet name table to index->name: the API ships
// a string-keyed object today, but an array shape (indexed by planet index)
// degrades to the same table instead of collapsing the roster.
func nameTable(raw any) map[string]string {
	out := map[string]string{}
	switch t := raw.(type) {
	case map[string]any:
		for k, v := range t {
			switch n := v.(type) {
			case map[string]any:
				if s := str(n, "name"); s != "" {
					out[k] = s
				}
			case string:
				if strings.TrimSpace(n) != "" {
					out[k] = n
				}
			}
		}
	case []any:
		for i, v := range t {
			key := strconv.Itoa(i)
			switch n := v.(type) {
			case map[string]any:
				if _, ok := n["index"]; ok {
					key = strconv.Itoa(int(num(n, "index")))
				}
				if s := str(n, "name"); s != "" {
					out[key] = s
				}
			case string:
				if strings.TrimSpace(n) != "" {
					out[key] = n
				}
			}
		}
	}
	return out
}

// isDefense reports whether a campaign entry is a defense: the API emits a
// bool today, but a numeric-tolerant read keeps a 0/1 shape from
// misclassifying into a campaign.
func isDefense(m map[string]any) bool {
	if defense, _ := m["defense"].(bool); defense {
		return true
	}
	return num(m, "defense") == 1
}

// ownerLabel maps the official owner code to the display owner.
func ownerLabel(code int) string {
	switch code {
	case 1:
		return "Humans"
	case 2:
		return "Terminids"
	case 3:
		return "Automatons"
	case 4:
		return "Illuminate"
	}
	return "Unknown"
}

// joinRoster folds the four war payloads into the planet roster PlanetLine
// and WarStatus read: one entry per status planet with its display owner,
// liberation health, divers, and — for contested planets — the live event.
// Returns nil when the payload looks stale (tiny roster or a galaxy with
// zero divers) so the outage path keeps the cache instead of printing a
// false dead galaxy.
func joinRoster(campaign []map[string]any, status, info map[string]any, names map[string]string) []map[string]any {
	maxByIndex := map[int]float64{}
	if list, ok := info["planetInfos"].([]any); ok {
		for _, e := range list {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := m["index"]; !ok {
				continue
			}
			maxByIndex[int(num(m, "index"))] = num(m, "maxHealth")
		}
	}
	campByIndex := map[int]map[string]any{}
	for _, e := range campaign {
		if _, ok := e["planetIndex"]; !ok {
			continue
		}
		campByIndex[int(num(e, "planetIndex"))] = e
	}
	statusList, _ := status["planetStatus"].([]any)
	plans := make([]map[string]any, 0, len(statusList))
	for _, e := range statusList {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := m["index"]; !ok {
			continue
		}
		idx := int(num(m, "index"))
		name := names[strconv.Itoa(idx)]
		camp := campByIndex[idx]
		if name == "" {
			// New or unlisted index: the campaign entry still names it.
			name = str(camp, "name")
		}
		if strings.TrimSpace(name) == "" {
			// Nameless payload entries carry no addressable planet.
			continue
		}
		p := map[string]any{
			"name":         name,
			"currentOwner": ownerLabel(int(num(m, "owner"))),
			"health":       num(m, "health"),
			"maxHealth":    maxByIndex[idx],
			"players":      num(m, "players"),
		}
		if camp != nil {
			kind := 0.0
			if isDefense(camp) {
				kind = 1.0
			}
			ev := map[string]any{
				"eventType": kind,
				"faction":   str(camp, "faction"),
				"health":    num(camp, "health"),
				"maxHealth": num(camp, "maxHealth"),
			}
			if exp := num(camp, "expireDateTime"); exp > 0 {
				sec, frac := math.Modf(exp)
				ev["endTime"] = time.Unix(int64(sec), int64(frac*1e9)).UTC().Format(time.RFC3339Nano)
			}
			p["event"] = ev
		}
		plans = append(plans, p)
	}
	if len(plans) < minRoster {
		return nil
	}
	var total float64
	for _, p := range plans {
		total += num(p, "players")
	}
	if total == 0 {
		return nil
	}
	return plans
}

// Up reports whether the war uplink is serving a usable roster: true when a
// cached or fresh fetch yields planets. It lets callers tell an outage
// (report the uplink down) apart from an unknown planet name.
func (c *Client) Up(ctx context.Context) bool {
	return len(c.planets(ctx)) > 0
}

func num(m map[string]any, key string) float64 {
	return toFloat(m[key])
}

// toFloat coerces one decoded JSON scalar to a float, tolerating the
// int/float/string shapes a mirror or a test may ship (encoding/json gives
// float64 in production). NaN, Inf, and unparseable input read as 0 so a
// malformed field can never poison arithmetic.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0
		}
		return n
	case float32:
		if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
			return 0
		}
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return 0
			}
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return 0
			}
			return f
		}
	}
	return 0
}

// parseEndTime parses the war API's endTime, which may carry fractional
// seconds: a single RFC3339Nano parse covers both shapes (the fractional
// part is optional), so the date never silently drops on a millis timestamp.
func parseEndTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// PlanetLine returns "Name: owner pct% — N divers." for an exact name,
// appending "DEFENSE vs <faction>: <repelled>% repelled, ends <date>."
// while an attack event is live. Without an event it ends at "divers.".
// Every return ends with "Over." so the model can quote it verbatim.
func (c *Client) PlanetLine(ctx context.Context, name string) string {
	line, _ := c.PlanetLineUp(ctx, name)
	return line
}

// PlanetLineUp returns the planet line and the uplink state in a single
// fetch, so a miss costs one roster read, not two: a non-empty line is the
// answer; an empty line with up=false means the uplink is down, while
// up=true means the name is simply unknown.
func (c *Client) PlanetLineUp(ctx context.Context, name string) (string, bool) {
	plans := c.planets(ctx)
	if len(plans) == 0 {
		return "", false
	}
	return planetLine(plans, name), true
}

func planetLine(plans []map[string]any, name string) string {
	for _, p := range plans {
		n, _ := p["name"].(string)
		if !strings.EqualFold(n, strings.TrimSpace(name)) {
			continue
		}
		owner, _ := p["currentOwner"].(string)
		max, health := num(p, "maxHealth"), num(p, "health")
		pct := 0.0
		if max > 0 {
			pct = 100 * (1 - health/max)
		}
		line := fmt.Sprintf("%s: %s %.1f%% — %.0f divers.", n, owner, pct, num(p, "players"))
		if ev, ok := p["event"].(map[string]any); ok && ev != nil {
			faction, _ := ev["faction"].(string)
			emax, ehealth := num(ev, "maxHealth"), num(ev, "health")
			if faction != "" && emax > 0 {
				kind := "campaign"
				if num(ev, "eventType") == 1 {
					kind = "DEFENSE"
				}
				end := ""
				if es, _ := ev["endTime"].(string); es != "" {
					if t, ok := parseEndTime(es); ok {
						end = ", ends " + t.Format("Jan 2")
					}
				}
				repelled := 100 * (1 - ehealth/emax)
				line += fmt.Sprintf(" %s vs %s: %.1f%% repelled%s.", kind, faction, repelled, end)
			}
		}
		return line + " Over."
	}
	return ""
}

// factionLabel normalizes an API faction name to the plural display form so
// tallies read the same whatever casing the API returns ("Automaton" vs
// "automatons"). Unknown factions pass through untouched.
func factionLabel(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "automaton", "automatons":
		return "Automatons"
	case "terminid", "terminids":
		return "Terminids"
	case "illuminate", "illuminates":
		return "Illuminate"
	}
	return strings.TrimSpace(s)
}

// warFront is one active event, flattened for sorting and one-line display.
type warFront struct {
	name, label, kind, end string
	pct, players           float64
}

// WarStatus summarizes the live war for a generic "war status" question that
// names no planet: active fronts per faction plus the three busiest battles.
// This is the tool that keeps the model from asking the user for a target.
// Empty return means the API gave us nothing at all (outage, no cache), so
// the caller can report a dead uplink instead of inventing a quiet galaxy.
func (c *Client) WarStatus(ctx context.Context) string {
	planets := c.planets(ctx)
	if len(planets) == 0 {
		return ""
	}
	var fronts []warFront
	counts := map[string]int{}
	divers := 0.0
	for _, p := range planets {
		divers += num(p, "players")
		ev, ok := p["event"].(map[string]any)
		if !ok || ev == nil {
			continue
		}
		faction, _ := ev["faction"].(string)
		emax, ehealth := num(ev, "maxHealth"), num(ev, "health")
		if faction == "" || emax <= 0 {
			continue
		}
		name := str(p, "name")
		if strings.TrimSpace(name) == "" {
			// Nameless payload entries carry no addressable front.
			continue
		}
		label := factionLabel(faction)
		counts[label]++
		f := warFront{
			name: name, label: label, kind: "campaign",
			pct: 100 * (1 - ehealth/emax), players: num(p, "players"),
		}
		if num(ev, "eventType") == 1 {
			f.kind = "DEFENSE"
		}
		if es, _ := ev["endTime"].(string); es != "" {
			if t, ok := parseEndTime(es); ok {
				f.end = t.Format("Jan 2")
			}
		}
		fronts = append(fronts, f)
	}
	if len(fronts) == 0 {
		return fmt.Sprintf("War status: no active fronts. %.0f divers deployed. Over.", divers)
	}
	// Busiest first; name breaks ties so repeats of one payload print the
	// same line instead of shuffling between calls.
	sort.SliceStable(fronts, func(i, j int) bool {
		if fronts[i].players != fronts[j].players {
			return fronts[i].players > fronts[j].players
		}
		return fronts[i].name < fronts[j].name
	})
	// Always tally all three known factions (zero included) so the answer
	// says which front is quiet, then any unexpected faction alphabetically.
	labels := []string{"Automatons", "Terminids", "Illuminate"}
	var extras []string
	for label := range counts {
		switch label {
		case "Automatons", "Terminids", "Illuminate":
		default:
			extras = append(extras, label)
		}
	}
	sort.Strings(extras)
	labels = append(labels, extras...)
	tally := make([]string, 0, len(labels))
	for _, label := range labels {
		tally = append(tally, fmt.Sprintf("%s %d", label, counts[label]))
	}
	parts := make([]string, 0, 3)
	for i, f := range fronts {
		if i == 3 {
			break
		}
		part := fmt.Sprintf("%s %s vs %s, %.1f%% ", f.name, strings.ToLower(f.kind), f.label, f.pct)
		if f.kind == "DEFENSE" {
			part += "repelled"
		} else {
			part += "liberated"
		}
		if f.end != "" {
			part += ", ends " + f.end
		}
		part += fmt.Sprintf(", %.0f divers", f.players)
		parts = append(parts, part)
	}
	noun := "fronts"
	if len(fronts) == 1 {
		noun = "front"
	}
	return fmt.Sprintf("War status: %d active %s — %s. Hottest: %s. %.0f divers deployed. Over.",
		len(fronts), noun, strings.Join(tally, ", "), strings.Join(parts, "; "), divers)
}

// MajorOrder answers "what is the Major Order" with the standing High
// Command objective: brief, kill/objective progress, expiry and reward.
// Companions (STRATEGIC THREAT tasks) ride along, so a "strategic threat"
// question resolves too. Empty means the feed gave us nothing at all
// (outage, no cache), so the caller reports a dead uplink instead of
// inventing an objective. A live feed with no assignments says so
// explicitly — never an uplink-down.
func (c *Client) MajorOrder(ctx context.Context) string {
	snap := c.snapshot(ctx)
	if len(snap.plans) == 0 {
		return ""
	}
	now := time.Now()
	// The MAJOR ORDER title is the contract: a threats-only feed says no
	// order is standing and reports the threats, rather than headlining a
	// STRATEGIC THREAT as the Major Order.
	idx := findMajorOrderIdx(snap.mo)
	line := ""
	if idx >= 0 {
		line = formatAssignment(snap.mo[idx], snap.names, now)
	}
	// Companions are capped at three so a growing feed can never blow the
	// Discord cut. Entries without a human brief carry nothing quotable;
	// title-less ones are skipped rather than given a default headline.
	companions := 0
	for i, a := range snap.mo {
		if i == idx {
			continue
		}
		setting, _ := a["setting"].(map[string]any)
		if strings.TrimSpace(str(setting, "overrideBrief")) == "" {
			continue
		}
		if strings.TrimSpace(str(setting, "overrideTitle")) == "" {
			continue
		}
		if companions >= 3 {
			break
		}
		companions++
		if line == "" {
			line = formatAssignment(a, snap.names, now)
			continue
		}
		line += " " + formatAssignment(a, snap.names, now)
	}
	if line == "" {
		return "No standing Major Order on the wire right now. Over."
	}
	if idx < 0 {
		line = "No standing Major Order. " + line
	}
	return line + " Over."
}

// findMajorOrderIdx returns the position of the entry titled MAJOR ORDER,
// or -1. Title-only on purpose: the feed carries strategic threats beside
// the order, and promoting the first brief-carrying entry would answer a
// Major Order question with a threat headline.
func findMajorOrderIdx(mo []map[string]any) int {
	for i, a := range mo {
		setting, _ := a["setting"].(map[string]any)
		if strings.EqualFold(strings.TrimSpace(str(setting, "overrideTitle")), "major order") {
			return i
		}
	}
	return -1
}

// fvalAt reads a positional number from a task values/progress list:
// the feed ships floats, but ints and numeric strings ride the same JSON
// through mirrors and tests, so toFloat handles every shape. Short lists
// read 0 instead of panicking.
func fvalAt(list []any, i int) float64 {
	if i < 0 || i >= len(list) {
		return 0
	}
	return toFloat(list[i])
}

// firstReward names the headline reward: type 1 is medals (the only reward
// the feed has ever paid on assignments); anything else prints generically
// rather than guessing a currency.
func firstReward(setting map[string]any) string {
	list, _ := setting["rewards"].([]any)
	if len(list) == 0 {
		return ""
	}
	rw, ok := list[0].(map[string]any)
	if !ok {
		return ""
	}
	if amount := num(rw, "amount"); amount > 0 {
		if int(num(rw, "type")) == 1 {
			return fmt.Sprintf("Reward: %.0f medals", amount)
		}
		return fmt.Sprintf("Reward: %.0f", amount)
	}
	return ""
}

// formatAssignment renders one assignment: title + brief, planet
// objectives resolved against the name table, kill progress paired by
// task position (progress[i] counts toward tasks[i]; the type-3 kill
// target rides values[2]), expiry, and reward. No sign-off: the caller
// terminates the joined line once, so companions never double it.
func formatAssignment(a map[string]any, names map[string]string, now time.Time) string {
	setting, _ := a["setting"].(map[string]any)
	title := strings.TrimSpace(str(setting, "overrideTitle"))
	if title == "" {
		title = "Major Order"
	}
	brief := strings.TrimSpace(str(setting, "overrideBrief"))
	if brief == "" {
		brief = strings.TrimSpace(str(setting, "taskDescription"))
	}
	if brief == "" {
		brief = title
	}
	brief = strings.TrimRight(brief, ".")
	var tasks []any
	if tl, ok := setting["tasks"].([]any); ok {
		tasks = tl
	}
	var progress []any
	if pl, ok := a["progress"].([]any); ok {
		progress = pl
	}
	var parts []string
	var worlds []string
	for i, raw := range tasks {
		task, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		vals, _ := task["values"].([]any)
		switch int(num(task, "type")) {
		case 3: // kill count: values[2] carries the mandated number.
			target := fvalAt(vals, 2)
			if target <= 0 {
				continue
			}
			done := fvalAt(progress, i)
			parts = append(parts, fmt.Sprintf("Progress %.0f of %.0f (%.1f%%)", done, target, 100*done/target))
		case 13: // planet objective: values[2] is the planet index.
			if name := names[strconv.Itoa(int(fvalAt(vals, 2)))]; strings.TrimSpace(name) != "" {
				worlds = append(worlds, strings.TrimSpace(name))
			}
		}
	}
	line := title + ": " + brief + "."
	if len(worlds) > 0 {
		line += " Objectives: " + strings.Join(worlds, ", ") + "."
	}
	for _, p := range parts {
		line += " " + p + "."
	}
	if exp := num(a, "expiresIn"); exp > 0 {
		line += " Ends " + now.Add(time.Duration(exp*float64(time.Second))).Format("Jan 2") + "."
	}
	if rw := firstReward(setting); rw != "" {
		line += " " + rw + "."
	}
	return line
}
