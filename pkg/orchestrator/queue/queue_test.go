package queue

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdd_CoalescesPendingKey: N adds of a pending key deliver exactly once.
func TestAdd_CoalescesPendingKey(t *testing.T) {
	q := New("test", nil)
	for i := 0; i < 100; i++ {
		q.Add("ns/svc")
	}
	assert.Equal(t, 1, q.Len(), "duplicate adds of a pending key must coalesce")

	key, shutdown := q.Get()
	require.False(t, shutdown)
	assert.Equal(t, "ns/svc", key)
	q.Done(key)

	assert.Equal(t, 0, q.Len(), "no residual deliveries after coalesced adds")
	s := q.Stats()
	assert.Equal(t, uint64(1), s.Adds)
	assert.Equal(t, uint64(99), s.Coalesced)
}

// TestAdd_DuringProcessingRedeliversOnce: a key re-added while a worker holds
// it is re-delivered exactly once after Done — the coalescing contract that
// guarantees "spec changed mid-reconcile" always triggers a follow-up run.
func TestAdd_DuringProcessingRedeliversOnce(t *testing.T) {
	q := New("test", nil)
	q.Add("ns/svc")

	key, _ := q.Get() // key now processing
	require.Equal(t, "ns/svc", key)

	// Re-add several times mid-processing: must coalesce to ONE re-delivery.
	q.Add("ns/svc")
	q.Add("ns/svc")
	q.Add("ns/svc")
	assert.Equal(t, 0, q.Len(), "re-adds during processing must not enter the pending queue")

	q.Done(key)
	assert.Equal(t, 1, q.Len(), "dirty key must be requeued exactly once after Done")

	key2, _ := q.Get()
	assert.Equal(t, "ns/svc", key2)
	q.Done(key2)
	assert.Equal(t, 0, q.Len())
}

// TestGet_NeverTwoWorkersSameKey is the core single-writer invariant: under a
// storm of re-adds with many workers, the handler never runs concurrently for
// the same key. An atomic in-flight counter per key detects any overlap.
func TestGet_NeverTwoWorkersSameKey(t *testing.T) {
	q := New("test", NewItemExponentialRateLimiter(time.Millisecond, 10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const keys = 4
	var inFlight [keys]int32
	var overlaps int32
	var processed int64

	var workWG sync.WaitGroup
	workWG.Add(1)
	go func() {
		defer workWG.Done()
		q.Work(ctx, 8, func(_ context.Context, key string) error {
			var idx int
			_, err := fmt.Sscanf(key, "ns/svc-%d", &idx)
			if err != nil {
				t.Errorf("bad key %q", key)
				return nil
			}
			if atomic.AddInt32(&inFlight[idx], 1) != 1 {
				atomic.AddInt32(&overlaps, 1)
			}
			time.Sleep(time.Microsecond) // widen any overlap window
			atomic.AddInt32(&inFlight[idx], -1)
			atomic.AddInt64(&processed, 1)
			return nil
		})
	}()

	// Hammer: 8 producers × 1k adds across 4 keys.
	var prodWG sync.WaitGroup
	for p := 0; p < 8; p++ {
		prodWG.Add(1)
		go func(p int) {
			defer prodWG.Done()
			for i := 0; i < 1000; i++ {
				q.Add(fmt.Sprintf("ns/svc-%d", (p+i)%keys))
			}
		}(p)
	}
	prodWG.Wait()

	// Let the workers drain, then stop.
	require.Eventually(t, func() bool { return q.Len() == 0 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond) // allow in-flight handlers to finish
	cancel()
	workWG.Wait()

	assert.Zero(t, atomic.LoadInt32(&overlaps),
		"the same key must never be processed by two workers concurrently")
	assert.Greater(t, atomic.LoadInt64(&processed), int64(0))
	t.Logf("processed %d runs for %d adds (coalescing working)", processed, 8*1000)
}

// TestAddAfter_DelaysDelivery: a delayed add is not deliverable before its
// delay and is deliverable after.
func TestAddAfter_DelaysDelivery(t *testing.T) {
	q := New("test", nil)
	q.AddAfter("ns/svc", 50*time.Millisecond)

	assert.Equal(t, 0, q.Len(), "key must not be pending before its delay")
	require.Eventually(t, func() bool { return q.Len() == 1 }, time.Second, 5*time.Millisecond,
		"key must become pending after the delay")

	// d <= 0 is immediate.
	q2 := New("test2", nil)
	q2.AddAfter("ns/now", 0)
	assert.Equal(t, 1, q2.Len())
}

// TestRateLimiter_BackoffGrowsAndForgetResets verifies exponential growth and
// reset semantics.
func TestRateLimiter_BackoffGrowsAndForgetResets(t *testing.T) {
	rl := NewItemExponentialRateLimiter(5*time.Millisecond, 1000*time.Second)

	assert.Equal(t, 5*time.Millisecond, rl.When("k"))
	assert.Equal(t, 10*time.Millisecond, rl.When("k"))
	assert.Equal(t, 20*time.Millisecond, rl.When("k"))
	assert.Equal(t, 3, rl.NumRequeues("k"))

	// Independent per key.
	assert.Equal(t, 5*time.Millisecond, rl.When("other"))

	rl.Forget("k")
	assert.Equal(t, 0, rl.NumRequeues("k"))
	assert.Equal(t, 5*time.Millisecond, rl.When("k"), "Forget must reset the backoff")

	// Cap saturates.
	capped := NewItemExponentialRateLimiter(time.Second, 4*time.Second)
	capped.When("c") // 1s
	capped.When("c") // 2s
	capped.When("c") // 4s
	assert.Equal(t, 4*time.Second, capped.When("c"), "backoff must saturate at max")
}

// TestShutDown_UnblocksGetAndDrainsPending: pending keys are still delivered
// after ShutDown; blocked Get waiters wake with shutdown=true once drained.
func TestShutDown_UnblocksGetAndDrainsPending(t *testing.T) {
	q := New("test", nil)
	q.Add("ns/a")
	q.ShutDown()

	// Pending key still delivered.
	key, shutdown := q.Get()
	assert.False(t, shutdown)
	assert.Equal(t, "ns/a", key)
	q.Done(key)

	// Drained → shutdown signal.
	_, shutdown = q.Get()
	assert.True(t, shutdown)

	// A blocked waiter also wakes up.
	q2 := New("test2", nil)
	got := make(chan bool, 1)
	go func() {
		_, sd := q2.Get()
		got <- sd
	}()
	time.Sleep(20 * time.Millisecond) // let the goroutine block in Get
	q2.ShutDown()
	select {
	case sd := <-got:
		assert.True(t, sd, "blocked Get must wake with shutdown=true")
	case <-time.After(2 * time.Second):
		t.Fatal("Get did not unblock after ShutDown")
	}

	// Add after shutdown is a no-op.
	q2.Add("ns/late")
	assert.Equal(t, 0, q2.Len())
}

// TestWork_ErrorRequeuesWithBackoff_SuccessForgets: a failing handler is
// retried (rate-limited) until it succeeds; success resets its history.
func TestWork_ErrorRequeuesWithBackoff_SuccessForgets(t *testing.T) {
	rl := NewItemExponentialRateLimiter(time.Millisecond, 50*time.Millisecond)
	q := New("test", rl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	done := make(chan struct{})
	var workWG sync.WaitGroup
	workWG.Add(1)
	go func() {
		defer workWG.Done()
		q.Work(ctx, 2, func(_ context.Context, key string) error {
			n := atomic.AddInt32(&attempts, 1)
			if n < 4 {
				return fmt.Errorf("transient failure %d", n)
			}
			select {
			case <-done:
			default:
				close(done)
			}
			return nil
		})
	}()

	q.Add("ns/flaky")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler never succeeded; attempts=%d", atomic.LoadInt32(&attempts))
	}
	require.Eventually(t, func() bool {
		return rl.NumRequeues("ns/flaky") == 0
	}, time.Second, 5*time.Millisecond, "success must Forget the key's backoff history")

	cancel()
	workWG.Wait()

	assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(4))
	s := q.Stats()
	assert.GreaterOrEqual(t, s.Requeues, uint64(3), "each failure must count a requeue")
}

// TestWork_ContextCancelStopsWorkers: cancelling the context shuts the queue
// down and Work returns.
func TestWork_ContextCancelStopsWorkers(t *testing.T) {
	q := New("test", nil)
	ctx, cancel := context.WithCancel(context.Background())

	returned := make(chan struct{})
	go func() {
		q.Work(ctx, 4, func(context.Context, string) error { return nil })
		close(returned)
	}()

	cancel()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Work did not return after context cancellation")
	}
}

// TestStats_TracksDepthAndProcessing sanity-checks the observability counters.
func TestStats_TracksDepthAndProcessing(t *testing.T) {
	q := New("test", nil)
	q.Add("ns/a")
	q.Add("ns/b")
	q.Add("ns/a") // coalesced

	s := q.Stats()
	assert.Equal(t, 2, s.Depth)
	assert.Equal(t, 2, s.MaxDepth)
	assert.Equal(t, uint64(2), s.Adds)
	assert.Equal(t, uint64(1), s.Coalesced)
}
