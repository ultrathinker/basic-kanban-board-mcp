package auth

import (
	"sync"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// RateLimiter is a bounded, in-memory token-bucket per key. The store is
// capped so a malicious actor cannot grow memory by spraying unique keys
// (PLAN §8 invariant). Two independent buckets are exposed:
//   - AllowToken at RateLimitPerTokenPerMin
//   - AllowLogin at RateLimitLoginPerIPMin
//
// A 100-item batch counts as one AllowToken call: the rate limit lives in the
// HTTP middleware, not the service layer.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	maxKeys  int
	now      func() time.Time
}

// bucket is a sliding-window counter over the last minute. We use a coarse
// "events in the current minute" counter rather than a true sliding window
// because the numbers involved (600/min, 20/min) make the burst/edge effects
// irrelevant and the bookkeeping O(1).
type bucket struct {
	windowStart time.Time // truncated to the minute
	count       int
}

const rateLimitMaxKeys = 4096

// NewRateLimiter constructs a limiter. now is injected for tests.
func NewRateLimiter(now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		maxKeys: rateLimitMaxKeys,
		now:     now,
	}
}

// AllowToken reports whether one more request may proceed under the per-token
// rate limit. A batch counts as one. Key is the token name (a caller without a
// token should not call this).
func (r *RateLimiter) AllowToken(key string) bool {
	return r.allow("tok:"+key, domain.RateLimitPerTokenPerMin)
}

// AllowLogin reports whether one more login attempt may proceed under the
// per-IP rate limit. Key is the client IP (or its trusted-proxy replacement).
func (r *RateLimiter) AllowLogin(key string) bool {
	return r.allow("ip:"+key, domain.RateLimitLoginPerIPMin)
}

// allow is the shared core. Returns true when the request is permitted.
func (r *RateLimiter) allow(key string, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now().UTC()
	minute := now.Truncate(time.Minute)

	b, ok := r.buckets[key]
	if !ok || !b.windowStart.Equal(minute) {
		// Touching a brand-new key: cap the map so attacker-supplied keys
		// cannot grow memory unbounded.
		if !ok && len(r.buckets) >= r.maxKeys {
			r.evictOldestLocked(minute)
		}
		b = &bucket{windowStart: minute, count: 0}
		r.buckets[key] = b
	}
	if b.count >= limit {
		return false
	}
	b.count++
	return true
}

// evictOldestLocked drops any bucket whose window is already behind the
// current minute; that frees space without affecting active callers. Caller
// holds r.mu.
func (r *RateLimiter) evictOldestLocked(now time.Time) {
	for k, b := range r.buckets {
		if b.windowStart.Before(now) {
			delete(r.buckets, k)
		}
	}
	// If the map is still over the cap (everything is current-minute),
	// drop a deterministic slice to bound memory under load.
	if len(r.buckets) >= r.maxKeys {
		i := 0
		for k := range r.buckets {
			if i >= r.maxKeys/4 {
				break
			}
			delete(r.buckets, k)
			i++
		}
	}
}

// Sweep drops stale buckets. The middleware calls this opportunistically;
// the only correctness consequence of skipping it is that the LRU eviction
// runs later.
func (r *RateLimiter) Sweep() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC().Truncate(time.Minute)
	removed := 0
	for k, b := range r.buckets {
		if b.windowStart.Before(now) {
			delete(r.buckets, k)
			removed++
		}
	}
	return removed
}

// Size returns the current number of tracked keys. Tests use it to verify
// the bound holds under churn.
func (r *RateLimiter) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}
