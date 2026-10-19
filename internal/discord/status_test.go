package discord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vsreddyh/rouge_automaton/internal/ratelimit"
)

type fakeProber struct{ model, mongo string }

func (f fakeProber) probeModel(context.Context) string { return f.model }
func (f fakeProber) probeMongo(context.Context) string { return f.mongo }

func TestIsStatus(t *testing.T) {
	for _, q := range []string{"/status", "!status", "  /STATUS "} {
		if !isStatus(q) {
			t.Fatalf("must detect %q", q)
		}
	}
	if isStatus("/cooldown") || isStatus("status") || isStatus("") {
		t.Fatal("must not overmatch")
	}
}

func TestStatusReply(t *testing.T) {
	ctx := context.Background()
	l := ratelimit.New(10, 3, 2, nil)
	got := statusReply(fakeProber{"clear", "reachable"}, time.Now().Add(-90*time.Minute), "u1", l, ctx)
	for _, want := range []string{"Helldiver", "Over.", "clear", "reachable", "Gateway"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status must contain %q: %q", want, got)
		}
	}
	// One failed check degrades, never fails the whole command.
	got = statusReply(fakeProber{"dead", "unknown"}, time.Time{}, "u1", l, ctx)
	if !strings.Contains(got, "dead") || !strings.HasSuffix(got, "Over.") {
		t.Fatalf("degraded status must still report: %q", got)
	}
	// No secrets leak: even with real-looking deps, no URIs or tokens.
	for _, bad := range []string{"http", "token", "Bearer", "opencode.ai", "mongo://"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Fatalf("status leaks %q: %q", bad, got)
		}
	}
}

type slowProber struct{ delay time.Duration }

func (s slowProber) probeModel(ctx context.Context) string {
	select {
	case <-ctx.Done():
		return "unknown"
	case <-time.After(s.delay):
		return "clear"
	}
}
func (s slowProber) probeMongo(ctx context.Context) string {
	select {
	case <-ctx.Done():
		return "unknown"
	case <-time.After(s.delay):
		return "reachable"
	}
}

func TestProbeAllConcurrent(t *testing.T) {
	start := time.Now()
	model, mongo := probeAll(context.Background(), slowProber{200 * time.Millisecond})
	if model != "clear" || mongo != "reachable" {
		t.Fatalf("got %q/%q", model, mongo)
	}
	if el := time.Since(start); el > 350*time.Millisecond {
		t.Fatalf("probes must overlap, took %v", el)
	}
}

func TestStatusCache(t *testing.T) {
	now := time.Now()
	cache := map[string]cachedStatus{}
	if _, hit := statusCached(cache, "u", now); hit {
		t.Fatal("empty cache must miss")
	}
	cache["u"] = cachedStatus{at: now, text: "old answer Over."}
	if got, hit := statusCached(cache, "u", now.Add(5*time.Second)); !hit || got != "old answer Over." {
		t.Fatal("fresh entry must hit")
	}
	if _, hit := statusCached(cache, "u", now.Add(statusCacheTTL+time.Second)); hit {
		t.Fatal("stale entry must miss")
	}
}

func TestStatusBeltPartialSpare(t *testing.T) {
	ctx := context.Background()
	l := ratelimit.New(10, 3, 2, nil)
	_, _ = l.Allow("u-part", false) // 2 of 3 left: spare, not dry
	got := statusReply(fakeProber{"clear", "reachable"}, time.Now(), "u-part", l, ctx)
	if !strings.Contains(got, "spare") {
		t.Fatalf("partial belt must read spare: %q", got)
	}
}
