package discord

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vsreddyh/rouge_automaton/internal/ratelimit"
)

// isStatus reports whether q invokes the status command: /status or
// !status, case-insensitive. Like /cooldown it works outside threads,
// gated by the same mention/free/thread rules as every other message.
func isStatus(q string) bool {
	s := strings.ToLower(strings.TrimSpace(q))
	return s == "/status" || s == "!status"
}

// statusProber is the seam statusReply programs against: model and mongo
// probes that report clear/busy/dead/unknown-style strings. Real wiring
// lives on Bot; tests inject fakes.
type statusProber interface {
	probeModel(ctx context.Context) string
	probeMongo(ctx context.Context) string
}

func (b *Bot) probeModel(ctx context.Context) string {
	if b.Relay == nil {
		return "unknown"
	}
	if st := b.Relay.Probe(ctx); st != "" {
		return st
	}
	return "unknown"
}

func (b *Bot) probeMongo(ctx context.Context) string {
	if b.Tools == nil || b.Tools.DB == nil {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := b.Tools.DB.Client().Ping(ctx, nil); err != nil {
		return "unreachable"
	}
	return "reachable"
}

// probeAll runs model and mongo probes concurrently: serially they cost up
// to 13s (10s model + 3s mongo) inside the 15s /status budget.
func probeAll(ctx context.Context, p statusProber) (model, mongo string) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); model = p.probeModel(ctx) }()
	go func() { defer wg.Done(); mongo = p.probeMongo(ctx) }()
	wg.Wait()
	return model, mongo
}

// statusCache bounds /status relay spend: one live probe per caller per
// statusCacheTTL; faster repeats reuse the last answer with no model call.
const statusCacheTTL = 10 * time.Second

type cachedStatus struct {
	at   time.Time
	text string
}

// statusCached swaps text in when the caller has a fresh cached answer.
func statusCached(cache map[string]cachedStatus, userID string, now time.Time) (string, bool) {
	if c, ok := cache[userID]; ok && now.Sub(c.at) < statusCacheTTL {
		return c.text, true
	}
	return "", false
}

// statusReply builds the /status answer in voice: gateway + uptime, model,
// mongo, caller belt, watcher. One failed check degrades to "unknown",
// never fails the whole command. Never prints secrets — no tokens, no URIs.
// The belt counts messages at the caller's channel cost, like /cooldown.
func statusReply(p statusProber, boot time.Time, userID string, freeChannel bool, l *ratelimit.Limiter, ctx context.Context) string {
	up := "since boot"
	if !boot.IsZero() {
		up = boot.Format("Jan 2 15:04") + " (" + time.Since(boot).Round(time.Minute).String() + " ago)"
	}
	model, mongo := probeAll(ctx, p)
	belt := "unknown"
	if l != nil {
		cost := 1.0
		if freeChannel {
			cost = ratelimit.DefaultFreeCost
		}
		msgs, reset := l.State(userID, cost)
		if msgs <= 0 {
			eta := time.Until(reset).Round(time.Second)
			if eta < time.Second {
				eta = time.Second
			}
			belt = fmt.Sprintf("dry — resupply %s", eta)
		} else {
			belt = fmt.Sprintf("%d rounds spare", msgs)
		}
	}
	return fmt.Sprintf("Gateway hot, Helldiver. Booted %s. Model uplink: %s. Mongo: %s. Belt: %s. Watcher: unreported. Over.",
		up, model, mongo, belt)
}
