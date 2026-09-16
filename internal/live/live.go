// Package live serves Type-L galactic war status from the community API
// with a 5-minute cache. Ports bot/live.py.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vsreddyh/rouge_automaton/internal/config"
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

// PlanetLine returns "Name: owner pct% — N divers. Over." for an exact name.
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
		return fmt.Sprintf("%s: %s %.1f%% — %.0f divers. Over.", n, owner, pct, num(p, "players"))
	}
	return ""
}
