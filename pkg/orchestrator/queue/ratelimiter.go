package queue

import (
	"math"
	"sync"
	"time"
)

// RateLimiter computes per-item retry delays for AddRateLimited. Implementations
// must be safe for concurrent use.
type RateLimiter interface {
	// When returns the delay to apply before re-adding key, growing with each
	// consecutive call for the same key.
	When(key string) time.Duration
	// Forget resets key's failure history (called after a successful sync).
	Forget(key string)
	// NumRequeues reports how many times key has been rate-limited since the
	// last Forget.
	NumRequeues(key string) int
}

// itemExponentialRateLimiter backs off per item: base * 2^failures, capped at max.
// This mirrors client-go's ItemExponentialFailureRateLimiter semantics.
type itemExponentialRateLimiter struct {
	mu       sync.Mutex
	failures map[string]int
	base     time.Duration
	max      time.Duration
}

// NewItemExponentialRateLimiter returns a per-key exponential-backoff limiter.
func NewItemExponentialRateLimiter(base, max time.Duration) RateLimiter {
	return &itemExponentialRateLimiter{
		failures: make(map[string]int),
		base:     base,
		max:      max,
	}
}

// DefaultRateLimiter matches the client-go default: 5ms base doubling up to
// 1000s. The reconciler's periodic resync is the safety net for anything that
// backs off this far.
func DefaultRateLimiter() RateLimiter {
	return NewItemExponentialRateLimiter(5*time.Millisecond, 1000*time.Second)
}

func (r *itemExponentialRateLimiter) When(key string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	exp := r.failures[key]
	r.failures[key] = exp + 1

	// base * 2^exp, saturating at max (guard the shift against overflow).
	backoff := float64(r.base.Nanoseconds()) * math.Pow(2, float64(exp))
	if backoff > float64(r.max.Nanoseconds()) {
		return r.max
	}
	return time.Duration(backoff)
}

func (r *itemExponentialRateLimiter) Forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, key)
}

func (r *itemExponentialRateLimiter) NumRequeues(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures[key]
}
