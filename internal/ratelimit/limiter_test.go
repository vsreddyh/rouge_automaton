package ratelimit

import (
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
	l := New(6000, 6000, 2, clk)
	// Drain global with many users: per-user buckets are deep, global is 4x.
	denied := false
	for i := 0; i < 25000; i++ {
		if ok, _ := l.Allow("user", false); !ok {
			denied = true
			break
		}
	}
	if !denied {
		t.Fatal("global bucket must eventually deny a single heavy user")
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
	rem, _ := l.State("new")
	if rem != 2 {
		t.Fatalf("fresh state = burst, got %d", rem)
	}
	_, _ = l.Allow("new", false)
	rem, reset := l.State("new")
	if rem != 1 {
		t.Fatalf("after one call remaining=1, got %d", rem)
	}
	if !reset.After(*tp) {
		t.Fatal("reset must be in the future while quota is spent")
	}
}
