package store

import (
	"context"
	"sync"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateFunc_NoLostUpdatesUnderConcurrency proves the core Phase-1 property
// (RFC #129): UpdateFunc's read-modify-write is atomic per key, so concurrent
// writers don't lose each other's updates. 50 goroutines each increment Scale
// while 20 more concurrently rewrite a DIFFERENT field (Status). Every Scale
// increment must survive — the old Get→mutate-snapshot→Update path would drop
// many (each writer reads a stale Scale and the status writers clobber it,
// which is exactly the `rune restart` 1→0→hang bug).
func TestUpdateFunc_NoLostUpdatesUnderConcurrency(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, store.Create(ctx, types.ResourceTypeService, "ns", "svc",
		&types.Service{Name: "svc", Namespace: "ns", Scale: 0, Status: types.ServiceStatusRunning}))

	const incrementers = 50
	const statusWriters = 20
	var wg sync.WaitGroup

	for i := 0; i < incrementers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var s types.Service
			if err := store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &s, func() error {
				s.Scale++ // touch only our field, on freshly-read state
				return nil
			}); err != nil {
				t.Errorf("incrementer: %v", err)
			}
		}()
	}
	for i := 0; i < statusWriters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var s types.Service
			st := types.ServiceStatusPending
			if i%2 == 1 {
				st = types.ServiceStatusDeploying
			}
			if err := store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &s, func() error {
				s.Status = st // a different field — must not revert Scale
				return nil
			}); err != nil {
				t.Errorf("status writer: %v", err)
			}
		}(i)
	}
	wg.Wait()

	var got types.Service
	require.NoError(t, store.Get(ctx, types.ResourceTypeService, "ns", "svc", &got))
	assert.Equal(t, incrementers, got.Scale,
		"every Scale increment must survive concurrent status writes (no lost updates)")
}

// TestUpdateFunc_ReappliesOnConflict is the restart bug in miniature: a "scale
// up" sets Scale=1 while a concurrent "status" writer flips Status many times.
// The scale-up must not be reverted by any status write.
func TestUpdateFunc_ReappliesOnConflict(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, store.Create(ctx, types.ResourceTypeService, "ns", "svc",
		&types.Service{Name: "svc", Namespace: "ns", Scale: 0, Status: types.ServiceStatusRunning}))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var s types.Service
		require.NoError(t, store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &s, func() error {
			s.Scale = 1
			return nil
		}))
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			var s types.Service
			require.NoError(t, store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &s, func() error {
				s.Status = types.ServiceStatusPending
				return nil
			}))
		}
	}()
	wg.Wait()

	var got types.Service
	require.NoError(t, store.Get(ctx, types.ResourceTypeService, "ns", "svc", &got))
	assert.Equal(t, 1, got.Scale, "the scale-up must not be clobbered by concurrent status writes")
}
