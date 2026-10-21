package discord

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"rouge_automaton/internal/config"
	"rouge_automaton/internal/persona"
	"rouge_automaton/internal/ratelimit"
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

// tail returns the last n bytes of s for failure messages, guarding
// against short strings so a future case never panics on the slice.
func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// boundModelReply must cap long model output at 1900 runes on a line
// boundary with an in-voice trailer, and pass short replies through
// untouched so the 100-rune path stays byte-identical to cutRunes+Postprocess.
func TestBoundModelReply(t *testing.T) {
	// Short replies pass through untouched.
	if got := boundModelReply("hi"); got != "hi" {
		t.Fatalf("short must pass through: %q", got)
	}
	if got := boundModelReply(strings.Repeat("a", 100)); len(got) != 100 {
		t.Fatalf("100-rune reply must pass through untouched, got %d runes", utf8.RuneCountInString(got))
	}
	// Exact budget passes through untouched.
	exact := strings.Repeat("b", modelReplyBudget)
	if got := boundModelReply(exact); got != exact {
		t.Fatal("exact-budget reply must pass through untouched")
	}
	// One-line overrun hard-cuts at the budget plus trailer.
	longLine := strings.Repeat("c", modelReplyBudget+100)
	got := boundModelReply(longLine)
	if !strings.HasSuffix(got, truncationTrailer) {
		t.Fatalf("truncated reply must end with trailer: %q", tail(got, 60))
	}
	head := strings.TrimSuffix(got, "\n\n"+truncationTrailer)
	if utf8.RuneCountInString(head) != modelReplyBudget {
		t.Fatalf("one-line head must hard-cut at budget, got %d runes", utf8.RuneCountInString(head))
	}
	// Multiline overrun cuts at the last line break: only complete lines.
	var sb strings.Builder
	for i := 0; i < 400; i++ {
		sb.WriteString(strings.Repeat("x", 18) + "\n")
	}
	big := sb.String() // 400 lines x 19 runes = 7600 runes
	got = boundModelReply(big)
	if !strings.Contains(got, truncationTrailer) {
		t.Fatal("multiline overrun must carry the trailer")
	}
	head = strings.TrimSuffix(got, "\n\n"+truncationTrailer)
	if utf8.RuneCountInString(head) > modelReplyBudget {
		t.Fatalf("head exceeds budget: %d runes", utf8.RuneCountInString(head))
	}
	for _, ln := range strings.Split(head, "\n") {
		if ln != strings.Repeat("x", 18) {
			t.Fatalf("head must hold complete lines only, bad line %q", ln)
		}
	}
	// Full path: 4000-rune answer posts within Discord's ceiling ending
	// with trailer + sign-off.
	var big4k strings.Builder
	for i := 0; i < 200; i++ {
		big4k.WriteString(strings.Repeat("y", 18) + "\n")
	}
	final := persona.Postprocess(boundModelReply(big4k.String()))
	if utf8.RuneCountInString(final) > 2000 {
		t.Fatalf("final reply exceeds Discord ceiling: %d runes", utf8.RuneCountInString(final))
	}
	if !strings.Contains(final, truncationTrailer) || !strings.HasSuffix(final, "Over.") {
		t.Fatalf("final reply must end with trailer + sign-off: %q", tail(final, 80))
	}
	// 100-rune parity with the old cut-then-postprocess path.
	short := strings.Repeat("z", 100)
	if persona.Postprocess(boundModelReply(short)) != persona.Postprocess(cutRunes(short, 2000)) {
		t.Fatal("short reply must stay byte-identical to the old path")
	}
	// Multibyte-safe: hard cut never splits a rune.
	emoji := strings.Repeat("héllo—", 400) // 2400 runes, one line
	got = boundModelReply(emoji)
	head = strings.TrimSuffix(got, "\n\n"+truncationTrailer)
	if utf8.RuneCountInString(head) != modelReplyBudget || !utf8.ValidString(got) {
		t.Fatal("bound must cut by runes without splitting multibyte characters")
	}
}

func TestIsCooldown(t *testing.T) {
	for _, q := range []string{"/cooldown", "!cooldown", "  /COOLDOWN  ", "/cooldown please", "!cooldown now"} {
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
	got := b.cooldownReply("u-cool", false)
	if !strings.HasSuffix(got, "Over.") || !strings.Contains(got, "Helldiver") {
		t.Fatalf("cooldown must stay in voice: %q", got)
	}
	// Drain the caller, then the reply must carry the dry-magazine line.
	for i := 0; i < 5; i++ {
		_, _ = b.Limiter.Allow("u-cool", false)
	}
	if got := b.cooldownReply("u-cool", false); !strings.Contains(got, "resupply") {
		t.Fatalf("dry bucket must report resupply: %q", got)
	}
	// Partially spent belt still reads spare (regression: ETA-gated dry).
	b2 := &Bot{Cfg: config.Load(), Limiter: ratelimit.New(10, 3, 2, nil)}
	_, _ = b2.Limiter.Allow("u-part", false)
	if got := b2.cooldownReply("u-part", false); !strings.Contains(got, "spare") {
		t.Fatalf("partial belt must read spare: %q", got)
	}
	// Nil limiter never panics, reads full.
	b3 := &Bot{Cfg: config.Load()}
	if got := b3.cooldownReply("u-nil", false); !strings.Contains(got, "spare") {
		t.Fatalf("nil limiter must read full: %q", got)
	}
	// Free-channel cost: 1 token left reads dry on a free channel.
	b4 := &Bot{Cfg: config.Load(), Limiter: ratelimit.New(60, 2, 2, nil)}
	_, _ = b4.Limiter.Allow("u-free", false) // 1 token left
	if got := b4.cooldownReply("u-free", false); !strings.Contains(got, "spare") {
		t.Fatalf("normal channel with 1 token must read spare: %q", got)
	}
	if got := b4.cooldownReply("u-free", true); !strings.Contains(got, "resupply") {
		t.Fatalf("free channel with 1 token must read dry: %q", got)
	}
}

func TestTypingInterval(t *testing.T) {
	if typingInterval < 5*time.Second || typingInterval > 8*time.Second {
		t.Fatalf("typing tick must be 5-8s, got %v", typingInterval)
	}
}

func TestTypingLoop(t *testing.T) {
	var calls int
	tick := func(ch string) error {
		if ch != "reply-ch" {
			t.Errorf("typing must target reply channel, got %q", ch)
		}
		calls++
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { typingLoop(ctx, tick, "reply-ch"); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("typing loop must stop on ctx cancel")
	}
	if calls < 1 {
		t.Fatal("typing must fire immediately on receipt")
	}
	// Nil tick and empty channel are no-ops, never panics.
	typingLoop(context.Background(), nil, "x")
	typingLoop(context.Background(), func(string) error { return nil }, "")
}
