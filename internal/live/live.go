// Package live serves Type-L galactic war status from the community API
// with a 5-minute cache. Ports bot/live.py.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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

	mu    sync.RWMutex
	at    time.Time
	plans []map[string]any
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

func (c *Client) planets(ctx context.Context) []map[string]any {
	c.mu.RLock()
	cached, at := c.plans, c.at
	c.mu.RUnlock()
	if time.Since(at) < 5*time.Minute && cached != nil {
		return cached
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/planets", nil)
	if err != nil {
		return cached
	}
	req.Header.Set("X-Super-Client", c.Client)
	req.Header.Set("X-Super-Contact", c.Contact)
	req.Header.Set("User-Agent", "rouge-automaton/1.0")
	resp, err := c.http().Do(req)
	if err != nil {
		return cached
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return cached
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return cached
	}
	c.mu.Lock()
	c.plans, c.at = out, time.Now()
	c.mu.Unlock()
	return out
}

func num(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
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
	for _, p := range c.planets(ctx) {
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
					if t, err := time.Parse(time.RFC3339, es); err == nil {
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
		label := factionLabel(faction)
		counts[label]++
		f := warFront{
			name: str(p, "name"), label: label, kind: "campaign",
			pct: 100 * (1 - ehealth/emax), players: num(p, "players"),
		}
		if num(ev, "eventType") == 1 {
			f.kind = "DEFENSE"
		}
		if es, _ := ev["endTime"].(string); es != "" {
			if t, err := time.Parse(time.RFC3339, es); err == nil {
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
