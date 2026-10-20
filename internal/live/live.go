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
	// every call during an outage refires all four endpoints.
	retry time.Time
	plans []map[string]any
}

// New builds a Client from the bot config.
func New(cfg config.Config) *Client {
	return &Client{Base: strings.TrimSuffix(cfg.HD2APIBase, "/"), Contact: cfg.XSuperContact, Client: cfg.XSuperClient}
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
// outage does not refire all four endpoints on every call.
func (c *Client) planets(ctx context.Context) []map[string]any {
	c.mu.RLock()
	cached, at, retry := c.plans, c.at, c.retry
	c.mu.RUnlock()
	if time.Since(at) < 5*time.Minute && cached != nil {
		return cached
	}
	if time.Now().Before(retry) {
		return cached
	}
	if fresh := c.refresh(ctx); fresh != nil {
		c.mu.Lock()
		c.plans, c.at = fresh, time.Now()
		c.mu.Unlock()
		return fresh
	}
	c.mu.Lock()
	c.retry = time.Now().Add(30 * time.Second)
	c.mu.Unlock()
	return cached
}

// htmPaths are the Training Manual endpoints joined into one roster per
// refresh, all rooted at Base: campaign carries the active-front detail
// (faction, defense, expiry), status the galaxy-wide per-planet owners and
// player counts, info the per-planet max health, and planets the
// index-to-name table.
var htmPaths = []string{"/war/campaign", "/war/status", "/war/info", "/planets"}

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
// galaxy-wide) — returns nil so the caller keeps serving the cache, or
// reports the uplink down when there is no cache at all.
func (c *Client) refresh(ctx context.Context) []map[string]any {
	bodies := make([][]byte, len(htmPaths))
	errs := make([]error, len(htmPaths))
	var wg sync.WaitGroup
	for i, p := range htmPaths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			body, err := fetchBody(ctx, c.http(), c.Base+p, c.Client, c.Contact)
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
	return joinRoster(campaign, status, info, nameTable(namesRaw))
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
			switch n := v.(type) {
			case map[string]any:
				if s := str(n, "name"); s != "" {
					out[strconv.Itoa(i)] = s
				}
			case string:
				if strings.TrimSpace(n) != "" {
					out[strconv.Itoa(i)] = n
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
			if m, ok := e.(map[string]any); ok {
				maxByIndex[int(num(m, "index"))] = num(m, "maxHealth")
			}
		}
	}
	campByIndex := map[int]map[string]any{}
	for _, e := range campaign {
		campByIndex[int(num(e, "planetIndex"))] = e
	}
	statusList, _ := status["planetStatus"].([]any)
	plans := make([]map[string]any, 0, len(statusList))
	for _, e := range statusList {
		m, ok := e.(map[string]any)
		if !ok {
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
				ev["endTime"] = time.Unix(int64(sec), int64(frac*1e9)).UTC().Format(time.RFC3339)
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
	// encoding/json decodes numbers into float64, but a type switch keeps
	// DEFENSE classification and counts correct if the API ever emits an
	// int shape (or tests construct one): without it eventType == 1 reads
	// 0 and every defense misclassifies as campaign.
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return 0
}

// parseEndTime parses the war API's endTime, which may carry fractional
// seconds: RFC3339Nano first, plain RFC3339 as fallback, so the date never
// silently drops on a millis timestamp.
func parseEndTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	t, err := time.Parse(time.RFC3339, s)
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
