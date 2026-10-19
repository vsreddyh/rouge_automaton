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

// globalMultiplier sizes the shared bucket relative to one caller.
const globalMultiplier = 4

// DefaultFreeCost prices free-response-channel messages: double tokens,
// the tighter bucket the issue requires.
const DefaultFreeCost = 2.0

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
		freeCost = DefaultFreeCost
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

// sweepIdle drops caller buckets untouched for over 30 minutes. Called
// from Allow only once the map grows past sweepTrigger entries, so the
// steady-state cost is one length check per message.
const sweepTrigger = 1000
const sweepIdleAfter = 30 * time.Minute

func (l *Limiter) sweep(t time.Time) {
	for k, b := range l.users {
		if t.Sub(b.last) > sweepIdleAfter {
			delete(l.users, k)
		}
	}
}

// usersLen reports tracked caller count. Test seam for the sweep.
func (l *Limiter) usersLen() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.users)
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
	l.refill(l.global, l.perMin*globalMultiplier, l.perMin*globalMultiplier, t)
	if len(l.users) > sweepTrigger {
		l.sweep(t)
	}
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
		if eta := time.Duration((cost - l.global.tokens) / (l.perMin * globalMultiplier) * float64(time.Minute)); eta > retryAfter {
			retryAfter = eta
		}
	}
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return false, retryAfter.Round(time.Second)
}

// State reports affordable messages at the given cost — the scarcer of the
// caller and global buckets, so a globally-throttled caller never sees
// "spare" — plus the time the next message becomes affordable. reset is now
// when a message is already affordable: callers must gate dryness on
// msgs<=0, never on the ETA. Non-positive cost normalizes to 1.
func (l *Limiter) State(key string, cost float64) (msgs int, reset time.Time) {
	if cost <= 0 {
		cost = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	l.refill(l.global, l.perMin*globalMultiplier, l.perMin*globalMultiplier, t)
	// Scarcer bucket binds. A fresh caller counts as a full personal
	// bucket at burst; a drained global still gates them.
	tok, rate := l.global.tokens, l.perMin*globalMultiplier
	if b, ok := l.users[key]; ok {
		l.refill(b, l.perMin, l.burst, t)
		if b.tokens < tok {
			tok, rate = b.tokens, l.perMin
		}
	} else if l.burst < tok {
		tok, rate = l.burst, l.perMin
	}
	if tok < 0 {
		tok = 0
	}
	if tok >= cost {
		return int(tok / cost), t
	}
	return 0, l.nextToken(t, tok, cost, rate)
}

// nextToken returns when need tokens accrue from tok at rate/min, floored at
// a one-second ETA so replies never print "0s".
func (l *Limiter) nextToken(t time.Time, tok, need, rate float64) time.Time {
	next := t.Add(time.Duration((need - tok) / rate * float64(time.Minute)))
	if next.Sub(t) < time.Second {
		next = t.Add(time.Second)
	}
	return next
}
