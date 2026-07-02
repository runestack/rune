// Package queue provides a coalescing, rate-limited, per-key work queue with
// the same semantics as Kubernetes client-go's workqueue: a key added while
// pending is deduplicated, a key added while being processed is re-delivered
// exactly once after the current run finishes, and no two workers ever process
// the same key concurrently. This is the serialization backbone of the
// orchestrator's single-writer reconcile model (RFC #129 Phase 3): correctness
// against concurrent spec writers comes from the store's UpdateFunc CAS, while
// this queue guarantees reconciles of one service never interleave.
package queue

import (
	"context"
	"sync"
	"time"
)

// Queue is a coalescing per-key work queue. All methods are safe for
// concurrent use.
//
// Key lifecycle (client-go workqueue semantics):
//
//	Add(k)      : k ∉ dirty → dirty + pending(order); k ∈ dirty → no-op (coalesced);
//	              k ∈ processing → dirty only (re-delivered after Done)
//	Get()       : pops the oldest pending key → processing (removed from dirty)
//	Done(k)     : k leaves processing; if re-Added meanwhile (k ∈ dirty) → pending again
type Queue struct {
	name string

	mu   sync.Mutex
	cond *sync.Cond

	// order is the FIFO of pending keys. dirty holds every key that needs a
	// (re)run — pending keys and keys re-Added mid-processing. processing
	// holds keys currently held by a worker.
	order      []string
	dirty      map[string]struct{}
	processing map[string]struct{}

	rl           RateLimiter
	shuttingDown bool

	stats Stats
}

// Stats is a point-in-time snapshot of queue counters, for observability.
type Stats struct {
	Adds              uint64        // Add calls that enqueued or dirtied a key
	Coalesced         uint64        // Add calls absorbed by an already-dirty key
	Requeues          uint64        // AddRateLimited calls (failed syncs)
	Processed         uint64        // handler invocations completed (via Work)
	Depth             int           // current pending keys
	MaxDepth          int           // high-water mark of Depth
	WorkDurationTotal time.Duration // cumulative handler runtime (via Work)
}

// New creates a Queue. name is used for identification in logs/stats only.
// A nil rl gets the DefaultRateLimiter.
func New(name string, rl RateLimiter) *Queue {
	if rl == nil {
		rl = DefaultRateLimiter()
	}
	q := &Queue{
		name:       name,
		dirty:      make(map[string]struct{}),
		processing: make(map[string]struct{}),
		rl:         rl,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add marks key as needing a run. Duplicate adds of a pending key coalesce;
// adding a key that is currently being processed schedules exactly one re-run
// after the in-flight run completes. Add on a shut-down queue is a no-op.
func (q *Queue) Add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.shuttingDown {
		return
	}
	if _, exists := q.dirty[key]; exists {
		q.stats.Coalesced++
		return
	}
	q.dirty[key] = struct{}{}
	q.stats.Adds++
	if _, beingProcessed := q.processing[key]; beingProcessed {
		// Re-delivered by Done() once the current run finishes.
		return
	}
	q.order = append(q.order, key)
	if d := len(q.order); d > q.stats.MaxDepth {
		q.stats.MaxDepth = d
	}
	q.cond.Signal()
}

// AddAfter adds key after delay d (immediately when d <= 0). Implemented with
// one timer per call rather than a shared heap: Add's dirty-set dedup makes
// redundant timers harmless, and at orchestrator scale (tens of services) the
// simplicity wins. A timer firing after ShutDown lands on the no-op Add path.
func (q *Queue) AddAfter(key string, d time.Duration) {
	if d <= 0 {
		q.Add(key)
		return
	}
	time.AfterFunc(d, func() { q.Add(key) })
}

// AddRateLimited adds key after the rate limiter's backoff for it.
func (q *Queue) AddRateLimited(key string) {
	q.mu.Lock()
	q.stats.Requeues++
	q.mu.Unlock()
	q.AddAfter(key, q.rl.When(key))
}

// Forget clears key's backoff history. Call after a successful sync.
func (q *Queue) Forget(key string) {
	q.rl.Forget(key)
}

// Get blocks until a key is available or the queue shuts down. The returned
// key is exclusively held by the caller (no other Get returns it) until the
// caller invokes Done.
func (q *Queue) Get() (key string, shutdown bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.order) == 0 && !q.shuttingDown {
		q.cond.Wait()
	}
	if len(q.order) == 0 {
		// Shutting down and drained.
		return "", true
	}

	key = q.order[0]
	q.order = q.order[1:]
	delete(q.dirty, key)
	q.processing[key] = struct{}{}
	return key, false
}

// Done releases key after processing. If the key was re-Added while being
// processed, it is requeued for exactly one further run.
func (q *Queue) Done(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.processing, key)
	if _, isDirty := q.dirty[key]; isDirty {
		q.order = append(q.order, key)
		if d := len(q.order); d > q.stats.MaxDepth {
			q.stats.MaxDepth = d
		}
		q.cond.Signal()
	}
}

// Len returns the number of pending (not in-flight) keys.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}

// ShutDown stops the queue: pending keys already in the queue are still
// delivered, then Get returns shutdown=true to every waiter. Idempotent.
func (q *Queue) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.shuttingDown = true
	q.cond.Broadcast()
}

// Stats returns a snapshot of the queue counters.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.stats
	s.Depth = len(q.order)
	return s
}

// Work runs n workers calling handler for each key until ctx is cancelled or
// the queue is shut down, then blocks until all workers have exited. A handler
// error requeues the key with backoff; success resets its backoff. Per-key
// exclusivity is inherited from Get/Done: handler never runs concurrently for
// the same key.
func (q *Queue) Work(ctx context.Context, n int, handler func(ctx context.Context, key string) error) {
	// Translate context cancellation into queue shutdown so workers blocked
	// in Get() wake up.
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.ShutDown()
		case <-stopWatch:
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				key, shutdown := q.Get()
				if shutdown {
					return
				}
				start := time.Now()
				err := handler(ctx, key)
				elapsed := time.Since(start)

				q.mu.Lock()
				q.stats.Processed++
				q.stats.WorkDurationTotal += elapsed
				q.mu.Unlock()

				if err != nil {
					q.AddRateLimited(key)
				} else {
					q.Forget(key)
				}
				q.Done(key)
			}
		}()
	}
	wg.Wait()
	close(stopWatch)
}
