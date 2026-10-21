// Package catalog reads the public Helldivers gear catalog
// (helldivers-api.dionatha.com.br): a daily snapshot of helldivers.wiki.gg's
// gear pages republished as static JSON. Every item carries the wiki page it
// came from, so the gear shelves can be refreshed without asking the wiki
// anything — and without the wiki's rate limiter getting in the way.
//
// Only the static collection files are used. The catalog's /v1/query/* and
// /v1/search endpoints are the rate-limited ones and are never called.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBase is the catalog API root.
const DefaultBase = "https://helldivers-api.dionatha.com.br"

// DefaultUA identifies the bot to the catalog host. The catalog is free and
// public, but it republishes wiki content: naming ourselves and the wiki
// keeps the attribution honest.
const DefaultUA = "rouge-automaton-catalog (wiki account: vsreddyh; contact: shouryanreddyh@gmail.com; use=reference)"

// maxBodyBytes caps one catalog response: the whole catalog is ~1.4 MB, and
// a broken proxy must not balloon the sync.
const maxBodyBytes = 8 << 20

// Collections are the static files the sync reads, in fetch order. The gear
// shelves come first so a partial fetch still syncs the shelves that matter.
var Collections = []string{
	"weapons", "stratagems", "armors", "helmets", "armor-sets",
	"passives", "boosters", "warbonds", "weapon-traits",
}

// ShelfForCollection maps a catalog collection to the repo shelf it feeds.
// The empty string means the collection is context, not a shelf: passives,
// warbonds and traits enrich gear docs, they are not gear of their own.
func ShelfForCollection(coll string) string {
	switch coll {
	case "weapons":
		return "weapons"
	case "stratagems":
		return "stratagems"
	case "boosters":
		return "boosters"
	case "armors", "helmets", "armor-sets":
		return "armor"
	default:
		return ""
	}
}

// WikiRef is the wiki page an item was scraped from: the provenance the
// seeds never had.
type WikiRef struct {
	Title string   `json:"title"`
	URL   string   `json:"url"`
	Flags []string `json:"flags"`
}

// Item is one catalog entry. The fields are the union of every collection's
// shape; a weapon simply leaves the armor fields empty.
type Item struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Upcoming    bool     `json:"upcoming"`
	Aliases     []string `json:"aliases"`
	Description string   `json:"description"`

	Wiki WikiRef `json:"wiki"`

	// Weapons.
	Category    string         `json:"category"`
	Subcategory string         `json:"subcategory"`
	TraitIDs    []string       `json:"traitIds"`
	Firearm     map[string]any `json:"firearm"`
	Throwable   map[string]any `json:"throwable"`
	Melee       map[string]any `json:"melee"`
	Attacks     []any          `json:"attacks"`
	StatsRaw    map[string]any `json:"statsRaw"`

	// Stratagems.
	Kind                string           `json:"kind"`
	PermitType          string           `json:"permitType"`
	Availability        string           `json:"availability"`
	Code                []string         `json:"code"`
	CooldownS           any              `json:"cooldownS"`
	CallInTimeS         any              `json:"callInTimeS"`
	CallInTimeUpgradedS any              `json:"callInTimeUpgradedS"`
	Uses                any              `json:"uses"`
	UnlockLevel         any              `json:"unlockLevel"`
	ShipModules         []any            `json:"shipModules"`
	SupportWeapon       map[string]any   `json:"supportWeapon"`
	Backpack            map[string]any   `json:"backpack"`
	CooldownVariants    []map[string]any `json:"cooldownVariants"`

	// Armor.
	Weight       string   `json:"weight"`
	ArmorRating  any      `json:"armorRating"`
	Speed        any      `json:"speed"`
	StaminaRegen any      `json:"staminaRegen"`
	PassiveID    string   `json:"passiveId"`
	SetIDs       []string `json:"setIds"`
	ArmorID      string   `json:"armorId"`
	HelmetID     string   `json:"helmetId"`
	CapeID       string   `json:"capeId"`

	// Boosters.
	Effect string `json:"effect"`

	// Passives.
	Effects []string `json:"effects"`

	// Warbonds.
	Type              string `json:"type"`
	ReleaseDate       string `json:"releaseDate"`
	SuperCreditsClaim bool   `json:"superCreditsClaimable"`

	// Traits.
	WeaponIDs    []string `json:"weaponIds"`
	StratagemIDs []string `json:"stratagemIds"`

	// Provenance: where the item was obtainable (warbond page, superstore).
	Source map[string]any `json:"source"`
}

// Snapshot is one parsed catalog file.
type Snapshot struct {
	// Version is the catalog's own snapshot stamp (meta.dataVersion).
	Version string
	// Items are the entries in file order.
	Items []Item
	// Raw is the exact payload, kept for the Mongo cache so a 304 can be
	// answered from the cache without another request.
	Raw []byte
}

// Count returns the number of items in the snapshot.
func (s Snapshot) Count() int { return len(s.Items) }

// Parse decodes a catalog file (meta + data).
func Parse(raw []byte) (Snapshot, error) {
	var wire struct {
		Meta struct {
			DataVersion string `json:"dataVersion"`
			Count       int    `json:"count"`
		} `json:"meta"`
		Data []Item `json:"data"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Snapshot{}, fmt.Errorf("catalog parse: %w", err)
	}
	return Snapshot{Version: wire.Meta.DataVersion, Items: wire.Data, Raw: raw}, nil
}

// Client reads the catalog's static files.
type Client struct {
	Base string
	HTTP *http.Client
	// UA identifies the client; set from config in production.
	UA string
}

// New returns a client for the live catalog.
func New() *Client { return &Client{Base: DefaultBase, UA: DefaultUA} }

// NewWithUA returns a client with an operator-supplied User-Agent.
func NewWithUA(ua string) *Client {
	c := New()
	if strings.TrimSpace(ua) != "" {
		c.UA = ua
	}
	return c
}

func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimSuffix(c.Base, "/")
	}
	return DefaultBase
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return sharedHTTP
}

func (c *Client) userAgent() string {
	if c.UA != "" {
		return c.UA
	}
	return DefaultUA
}

// sharedHTTP is the process-wide client (keep-alive, single timeout).
var sharedHTTP = &http.Client{Timeout: 60 * time.Second}

// URL is the static file for one collection. The query and search endpoints
// are deliberately absent: those are the rate-limited ones.
func (c *Client) URL(coll string) string {
	return c.base() + "/v1/" + coll + ".json"
}

// FetchResult is one collection fetch.
type FetchResult struct {
	// Snapshot holds the parsed payload (empty when NotModified).
	Snapshot Snapshot
	// ETag is the validator for the next conditional request.
	ETag string
	// NotModified is true when the server answered 304: the snapshot is
	// unchanged, and the cached copy is still the current one.
	NotModified bool
}

// Fetch reads one collection. When etag is non-empty it is sent as
// If-None-Match, so an unchanged snapshot costs one 304 and no payload.
func (c *Client) Fetch(ctx context.Context, coll, etag string) (FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL(coll), nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return FetchResult{}, err
	}
	defer resp.Body.Close()
	newETag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		// A 304 often carries no ETag: keep the validator we sent, or
		// the next poll would download the whole payload again.
		if newETag == "" {
			newETag = etag
		}
		return FetchResult{ETag: newETag, NotModified: true}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return FetchResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, fmt.Errorf("catalog %s status %d%s", coll, resp.StatusCode, bodySnippet(body))
	}
	snap, err := Parse(body)
	if err != nil {
		return FetchResult{}, err
	}
	return FetchResult{Snapshot: snap, ETag: newETag}, nil
}

// bodySnippet keeps a short piece of an error body: a proxy error says
// why, and the reason is worth the log line.
func bodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return ": " + s
}

// normalize folds a name for matching: lower case, trimmed, underscores and
// spaces are the same thing to the wiki and to the catalog.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.ReplaceAll(s, "_", " ")
}
