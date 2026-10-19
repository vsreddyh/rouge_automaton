package discord

import (
	"strings"
	"testing"

	"github.com/vsreddyh/rouge_automaton/internal/config"
	"github.com/vsreddyh/rouge_automaton/internal/ratelimit"
)

// gatePass must admit exactly the intended matrix: open mode admits all,
// otherwise one of mention, free channel, or live thread session is needed.
func TestGatePass(t *testing.T) {
	cases := []struct {
		name                           string
		require, mentioned, free, live bool
		want                           bool
	}{
		{"open mode admits stranger chatter", false, false, false, false, true},
		{"mention admits", true, true, false, false, true},
		{"free channel admits", true, false, true, false, true},
		{"live thread admits", true, false, false, true, true},
		{"plain channel denies", true, false, false, false, false},
		{"all signals admits", true, true, true, true, true},
	}
	for _, c := range cases {
		if got := gatePass(c.require, c.mentioned, c.free, c.live); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// threadSet must be zero-value ready, drop empty IDs, and hit tracked ones.
func TestThreadSet(t *testing.T) {
	var ts threadSet
	if ts.has("nope") {
		t.Fatal("empty set must miss")
	}
	ts.add("")
	if ts.has("") {
		t.Fatal("empty ID must never hit")
	}
	ts.add("thread-1")
	if !ts.has("thread-1") {
		t.Fatal("tracked thread must hit")
	}
}

// cutRunes must cut by characters, never splitting multibyte runes, and
// pass short strings through untouched.
func TestCutRunes(t *testing.T) {
	if got := cutRunes("héllo—world", 5); got != "héllo" {
		t.Fatalf("rune cut: %q", got)
	}
	if cutRunes("hi", 5) != "hi" {
		t.Fatal("short string must pass through")
	}
}

func TestIsCooldown(t *testing.T) {
	for _, q := range []string{"/cooldown", "!cooldown", "  /COOLDOWN  "} {
		if !isCooldown(q) {
			t.Fatalf("must detect %q", q)
		}
	}
	if isCooldown("cooldown") || isCooldown("/status") || isCooldown("") {
		t.Fatal("must not overmatch")
	}
}

func TestCooldownReplyInVoice(t *testing.T) {
	b := &Bot{Cfg: config.Load(), Limiter: ratelimit.New(10, 3, 2, nil)}
	got := b.cooldownReply("u-cool")
	if !strings.HasSuffix(got, "Over.") || !strings.Contains(got, "Helldiver") {
		t.Fatalf("cooldown must stay in voice: %q", got)
	}
	// Drain the caller, then the reply must carry the dry-magazine line.
	for i := 0; i < 5; i++ {
		_, _ = b.Limiter.Allow("u-cool", false)
	}
	if got := b.cooldownReply("u-cool"); !strings.Contains(got, "resupply") {
		t.Fatalf("dry bucket must report resupply: %q", got)
	}
	// Partially spent belt still reads spare (regression: ETA-gated dry).
	b2 := &Bot{Cfg: config.Load(), Limiter: ratelimit.New(10, 3, 2, nil)}
	_, _ = b2.Limiter.Allow("u-part", false)
	if got := b2.cooldownReply("u-part"); !strings.Contains(got, "spare") {
		t.Fatalf("partial belt must read spare: %q", got)
	}
	// Nil limiter never panics, reads full.
	b3 := &Bot{Cfg: config.Load()}
	if got := b3.cooldownReply("u-nil"); !strings.Contains(got, "spare") {
		t.Fatalf("nil limiter must read full: %q", got)
	}
}
