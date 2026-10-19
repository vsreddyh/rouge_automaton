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
