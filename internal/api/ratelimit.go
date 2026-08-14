package api

import (
	"sync"
	"time"
)

// Per-identity rate limiting (issue #74, ADR-0024 §6).
//
// # In-memory, and honest about it
//
// The buckets live in this process. Two replicas would each grant the full
// budget, so the effective limit is per replica — which is the truth for a v0
// server that ADR-0013 §1 says holds no state a restart would lose, and it is
// stated here rather than papered over. A shared limiter is a store round trip
// per request in exchange for an exactness nothing yet needs; when kelson-server
// is genuinely replicated the bucket moves behind an interface and this file
// becomes the single-process implementation of it.
//
// What in-memory costs on a restart is that every agent's budget refills at
// once. That is the safe direction: a restart never makes a limit stricter than
// the operator asked for, and the request that lands in the window is one the
// identity was entitled to a moment earlier anyway.

// bucketIdleTTL is how long an unused bucket is kept. An identity that stopped
// calling should stop costing memory, and an identity that comes back after
// this long has certainly refilled.
const bucketIdleTTL = 10 * time.Minute

// bucketSweepAt is the population that triggers a sweep of idle buckets. The
// sweep is O(n) and runs under the lock, so it is tied to a size rather than to
// a timer: a server with three agents never pays for it.
const bucketSweepAt = 256

// bucket is one identity's token bucket: `tokens` refilled at `rate` per second
// up to `burst`.
type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is the process's set of buckets, keyed by identity name.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}}
}

// allow spends one token for name and reports whether there was one. A
// non-positive budget is treated as unlimited: the store's defaults mean an
// identity never has one, and refusing every request because a stored number
// was zero would turn a decoding accident into a total outage.
func (l *limiter) allow(name string, requestsPerMinute, burst int, now time.Time) bool {
	if requestsPerMinute <= 0 || burst <= 0 {
		return true
	}
	rate := float64(requestsPerMinute) / 60
	capacity := float64(burst)

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[name]
	if !ok {
		if len(l.buckets) >= bucketSweepAt {
			l.sweep(now)
		}
		// A new identity starts full: its first request is not the one that
		// pays for a bucket it never used.
		b = &bucket{tokens: capacity, last: now}
		l.buckets[name] = b
	}

	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * rate
		if b.tokens > capacity {
			b.tokens = capacity
		}
	}
	// A clock that went backwards (a test's fake, an NTP step) must not mint
	// tokens; leaving `last` alone means the next forward step refills from the
	// older instant, which errs towards the caller.
	if now.After(b.last) {
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets nothing has touched for [bucketIdleTTL]. It runs under
// the caller's lock.
func (l *limiter) sweep(now time.Time) {
	for name, b := range l.buckets {
		if now.Sub(b.last) > bucketIdleTTL {
			delete(l.buckets, name)
		}
	}
}
