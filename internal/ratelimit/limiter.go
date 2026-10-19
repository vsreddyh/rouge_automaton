// Package ratelimit is a client-side token-bucket guard in front of model
// calls. It burns no model budget itself: Allow checks per-user and global
// buckets, and Cooldown reports remaining quota for /cooldown replies.
package ratelimit

import (
	"sync"
	"time"
)

// Clock lets tests control time; nil means time.Now.
type Clock func() time.Time

// Limiter holds per-user and global token buckets with periodic refill.
// Zero value is not ready — use New.
type Limiter struct {
	mu       sync.Mutex
	perMin   float64
	burst    float64
	users    map[string]*bucket
	global   *bucket
	freeCost float64
	now      Clock
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds a limiter: perMin tokens refill per user per minute (and
// globally at 4x), burst caps accumulation. freeCost is the token price of
// free-response-channel messages (tighter bucket); 0 defaults to 2.
func New(perMin, burst int, freeCost float64, clk Clock) *Limiter {
	if perMin <= 0 {
		perMin = 10
	}
	if burst <= 0 {
		burst = 3
	}
	if freeCost <= 0 {
		freeCost = 2
	}
	now := clk
	if now == nil {
		now = time.Now
	}
	t := now()
	return &Limiter{
		perMin: float64(perMin), burst: float64(burst),
		users:    map[string]*bucket{},
		global:   &bucket{tokens: float64(perMin) * 4, last: t},
		freeCost: freeCost, now: now,
	}
}

// refill adds rate*elapsed tokens, capped at max.
func (l *Limiter) refill(b *bucket, rate, max float64, t time.Time) {
	if el := t.Sub(b.last).Minutes(); el > 0 {
		b.tokens += el * rate
		if b.tokens > max {
			b.tokens = max
		}
		b.last = t
	}
}

// Allow consumes cost tokens (1 normal, freeCost for free channels) from the
// caller's bucket and the global bucket. Thread follow-ups share the caller
// bucket: key must be the author ID, never the thread ID. Returns ok and the
// retry-after ETA on denial.
func (l *Limiter) Allow(key string, freeChannel bool) (ok bool, retryAfter time.Duration) {
	cost := 1.0
	if freeChannel {
		cost = l.freeCost
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	l.refill(l.global, l.perMin*4, l.perMin*4, t)
	b, ok := l.users[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: t}
		l.users[key] = b
	}
	l.refill(b, l.perMin, l.burst, t)
	if b.tokens >= cost && l.global.tokens >= cost {
		b.tokens -= cost
		l.global.tokens -= cost
		return true, 0
	}
	// ETA from the scarcest bucket: tokens still owed / refill rate.
	need := cost
	if b.tokens < need {
		need = cost - b.tokens
		if eta := time.Duration(need / l.perMin * float64(time.Minute)); eta > retryAfter {
			retryAfter = eta
		}
	}
	if l.global.tokens < cost {
		if eta := time.Duration((cost - l.global.tokens) / (l.perMin * 4) * float64(time.Minute)); eta > retryAfter {
			retryAfter = eta
		}
	}
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return false, retryAfter.Round(time.Second)
}

// State reports remaining whole tokens and reset time for /cooldown.
func (l *Limiter) State(key string) (remaining int, reset time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	b, ok := l.users[key]
	if !ok {
		return int(l.burst), t
	}
	l.refill(b, l.perMin, l.burst, t)
	if b.tokens < 0 {
		b.tokens = 0
	}
	fullIn := time.Duration((l.burst - b.tokens) / l.perMin * float64(time.Minute))
	return int(b.tokens), t.Add(fullIn)
}
