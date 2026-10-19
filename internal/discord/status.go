package discord

import (
	"context"
	"fmt"
	"strings"
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
// probes that report "online"/"reachable" or degrade to "unknown". Real
// wiring lives on Bot; tests inject fakes.
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

// statusReply builds the /status answer in voice: gateway + uptime, model,
// mongo, caller belt, watcher. One failed check degrades to "unknown",
// never fails the whole command. Never prints secrets — no tokens, no URIs.
func statusReply(p statusProber, boot time.Time, userID string, l *ratelimit.Limiter, ctx context.Context) string {
	up := "since boot"
	if !boot.IsZero() {
		up = boot.Format("Jan 2 15:04") + " (" + time.Since(boot).Round(time.Minute).String() + " ago)"
	}
	model := p.probeModel(ctx)
	mongo := p.probeMongo(ctx)
	belt := "unknown"
	if l != nil {
		if rem, reset := l.State(userID); true {
			if eta := time.Until(reset); rem <= 0 || eta > time.Second {
				belt = fmt.Sprintf("dry — resupply %s", eta.Round(time.Second))
			} else {
				belt = fmt.Sprintf("%d rounds spare", rem)
			}
		}
	}
	return fmt.Sprintf("Gateway hot, Helldiver. Booted %s. Model uplink: %s. Mongo: %s. Belt: %s. Watcher: unreported. Over.",
		up, model, mongo, belt)
}
