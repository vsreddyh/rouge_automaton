package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func testClock() (Clock, *time.Time) {
	t := time.Now()
	return func() time.Time { return t }, &t
}

func TestBurstAndRefill(t *testing.T) {
	clk, tp := testClock()
	l := New(60, 2, 2, clk) // 1 token/sec, burst 2
	if ok, _ := l.Allow("u1", false); !ok {
		t.Fatal("first must pass")
	}
	if ok, _ := l.Allow("u1", false); !ok {
		t.Fatal("burst second must pass")
	}
	if ok, _ := l.Allow("u1", false); ok {
		t.Fatal("over burst must deny")
	}
	*tp = tp.Add(61 * time.Second) // refill to full
	if ok, _ := l.Allow("u1", false); !ok {
		t.Fatal("after refill must pass")
	}
}

func TestGlobalVsPerUser(t *testing.T) {
	clk, _ := testClock()
	l := New(60, 6000, 2, clk)
	// Distinct keys per iteration so per-user buckets never bind: only the
	// shared global bucket (4x rate = 240/min) can deny.
	denied := false
	for i := 0; i < 300; i++ {
		if ok, _ := l.Allow(fmt.Sprintf("user-%d", i), false); !ok {
			denied = true
			break
		}
	}
	if !denied {
		t.Fatal("global bucket must eventually deny across many users")
	}
	// Fresh user is also gated once global is dry.
	clk2, _ := testClock()
	l2 := New(60, 1, 2, clk2)
	l2.global.tokens = 0
	if ok, _ := l2.Allow("fresh", false); ok {
		t.Fatal("dry global must deny even fresh users")
	}
}

func TestFreeChannelTighter(t *testing.T) {
	clk, _ := testClock()
	l := New(60, 2, 2, clk)
	if ok, _ := l.Allow("u", true); !ok {
		t.Fatal("first free must pass (cost 2 = full burst)")
	}
	if ok, _ := l.Allow("u", true); ok {
		t.Fatal("second free must deny (tighter bucket)")
	}
}

func TestState(t *testing.T) {
	clk, tp := testClock()
	l := New(60, 2, 2, clk)
	rem, reset := l.State("new", 1)
	if rem != 2 {
		t.Fatalf("fresh state = burst, got %d", rem)
	}
	if reset.After(*tp) {
		t.Fatalf("fresh reset must be now (token affordable), got %v", reset)
	}
	_, _ = l.Allow("new", false)
	// Partially spent: still spare, reset stays now — dryness gates on
	// remaining<=0, never on the ETA (regression: time-to-full lied here).
	rem, reset = l.State("new", 1)
	if rem != 1 {
		t.Fatalf("after one call remaining=1, got %d", rem)
	}
	if reset.After(*tp) {
		t.Fatalf("partial reset must be now, got %v", reset)
	}
	_, _ = l.Allow("new", false)
	rem, reset = l.State("new", 1)
	if rem != 0 {
		t.Fatalf("drained remaining=0, got %d", rem)
	}
	if !reset.After(*tp) {
		t.Fatal("drained reset must be in the future (next token ETA)")
	}
	if eta := reset.Sub(*tp); eta < time.Second || eta > 70*time.Second {
		t.Fatalf("next-token ETA out of range: %v", eta)
	}
}

func TestStateSeesGlobal(t *testing.T) {
	clk, _ := testClock()
	l := New(60, 2, 2, clk)
	l.global.tokens = 0
	if rem, _ := l.State("fresh", 1); rem != 0 {
		t.Fatalf("dry global must report 0 even for fresh callers, got %d", rem)
	}
}

func TestStateFreshMinWithGlobal(t *testing.T) {
	clk, _ := testClock()
	l := New(60, 3, 2, clk) // burst 3
	l.global.tokens = 1.5
	// Fresh caller: burst says 3, global affords 1 — report the min.
	if rem, _ := l.State("fresh", 1); rem != 1 {
		t.Fatalf("fresh state must min burst with global, got %d", rem)
	}
}

func TestStateCost(t *testing.T) {
	clk, tp := testClock()
	l := New(60, 3, 2, clk)
	if msgs, _ := l.State("u", 2); msgs != 1 {
		t.Fatalf("3 tokens at cost 2 = 1 msg, got %d", msgs)
	}
	if msgs, reset := l.State("u", 1000); msgs != 0 || !reset.After(*tp) {
		t.Fatalf("unaffordable cost must report 0 with future ETA: %d/%v", msgs, reset)
	}
	if msgs, _ := l.State("u", 0); msgs != 3 {
		t.Fatalf("non-positive cost normalizes to 1, got %d", msgs)
	}
}

func TestStateMaxEta(t *testing.T) {
	clk, tp := testClock()
	l := New(2, 10, 2, clk) // user refills 2/min, global 8/min
	_, _ = l.Allow("u", false)
	// Drain the user bucket by hand; global stays full. ETA must come from
	// the user bucket (~15s), not the faster global rate.
	l.mu.Lock()
	l.users["u"].tokens = 0.5
	l.mu.Unlock()
	_, reset := l.State("u", 1)
	if eta := reset.Sub(*tp); eta < 12*time.Second || eta > 18*time.Second {
		t.Fatalf("user-bound ETA must be ~15s, got %v", eta)
	}
	// Flip it: full user bucket, dry global. ETA must come from global.
	l.mu.Lock()
	l.users["u"].tokens = 10
	l.global.tokens = 0
	l.mu.Unlock()
	_, reset = l.State("u", 1)
	if eta := reset.Sub(*tp); eta < 5*time.Second || eta > 10*time.Second {
		t.Fatalf("global-bound ETA must be ~7.5s, got %v", eta)
	}
}

func TestSweepIdle(t *testing.T) {
	clk, tp := testClock()
	l := New(60, 2, 2, clk)
	for i := 0; i < sweepTrigger+10; i++ {
		_, _ = l.Allow(fmt.Sprintf("idle-%d", i), false)
	}
	if n := l.usersLen(); n != sweepTrigger+10 {
		t.Fatalf("want %d tracked, got %d", sweepTrigger+10, n)
	}
	*tp = tp.Add(sweepIdleAfter + time.Minute)
	_, _ = l.Allow("new-guy", false) // trips the sweep, then tracks itself
	if n := l.usersLen(); n != 1 {
		t.Fatalf("sweep must evict idle buckets, tracked=%d", n)
	}
}
