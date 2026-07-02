package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type watchTestResource struct {
	Name string `json:"name"`
	Seq  int    `json:"seq"`
}

// TestWatch_NoDropsUnderBurst proves the watch bus is non-lossy: a burst far
// larger than any internal buffer (the old design dropped past a 100-deep main
// channel + 10-deep per-watcher channel) is delivered in full and in order,
// even though the consumer drains a little behind the producer.
func TestWatch_NoDropsUnderBurst(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := store.Watch(ctx, types.ResourceTypeService, "default")
	require.NoError(t, err)

	const total = 500 // >> old 100+10 buffers; old design would drop
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("svc-%d", i)
		require.NoError(t, store.Create(ctx, types.ResourceTypeService, "default", name,
			watchTestResource{Name: name, Seq: i}))
	}

	deadline := time.After(10 * time.Second)
	for i := 0; i < total; i++ {
		select {
		case ev := <-events:
			assert.Equal(t, WatchEventCreated, ev.Type)
			assert.Equal(t, fmt.Sprintf("svc-%d", i), ev.Name,
				"events must arrive in order with no gaps (drop)")
		case <-deadline:
			t.Fatalf("timed out after receiving %d/%d events (dropped events)", i, total)
		}
	}
}

// TestWatch_SlowConsumerDoesNotStarveOthers proves per-subscriber isolation: a
// stuck consumer on the same scope grows only its own backlog and never blocks
// or drops events for a healthy consumer. The old single-handler design could
// not starve peers (it dropped instead), but a naive "block until delivered"
// fix would — this guards against that regression.
func TestWatch_SlowConsumerDoesNotStarveOthers(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// stuckCtx lets us tear down the never-draining watcher cleanly at the end.
	stuckCtx, stuckCancel := context.WithCancel(ctx)
	defer stuckCancel()

	stuck, err := store.Watch(stuckCtx, types.ResourceTypeService, "default")
	require.NoError(t, err)
	_ = stuck // intentionally never read from

	healthy, err := store.Watch(ctx, types.ResourceTypeService, "default")
	require.NoError(t, err)

	const total = 300
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("svc-%d", i)
		require.NoError(t, store.Create(ctx, types.ResourceTypeService, "default", name,
			watchTestResource{Name: name, Seq: i}))
	}

	deadline := time.After(10 * time.Second)
	for i := 0; i < total; i++ {
		select {
		case ev := <-healthy:
			assert.Equal(t, fmt.Sprintf("svc-%d", i), ev.Name)
		case <-deadline:
			t.Fatalf("healthy consumer starved by stuck peer: got %d/%d", i, total)
		}
	}
}

// TestWatch_ClosesOnContextCancel verifies a watcher's channel is closed once
// its context is cancelled, so ranging consumers observe the shutdown.
func TestWatch_ClosesOnContextCancel(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	events, err := store.Watch(ctx, types.ResourceTypeService, "default")
	require.NoError(t, err)

	cancel()

	select {
	case _, ok := <-events:
		assert.False(t, ok, "channel must be closed after context cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("watch channel was not closed after context cancel")
	}
}
