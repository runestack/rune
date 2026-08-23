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

	const increments = 50
	const statusWriters = 20
	var wg sync.WaitGroup

	for i := 0; i < increments; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var s types.Service
			if err := store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &s, func() error {
				s.Scale++ // touch only our field, on freshly-read state
				return nil
			}); err != nil {
				t.Errorf("scale increment: %v", err)
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
	assert.Equal(t, increments, got.Scale,
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

// TestUpdateFunc_SkipUpdate verifies the ErrSkipUpdate sentinel aborts the write
// (no change, no error) — used by callers that decide, after seeing the fresh
// value, that no update is needed (e.g. status already set, resource deleted).
func TestUpdateFunc_SkipUpdate(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, store.Create(ctx, types.ResourceTypeService, "ns", "svc",
		&types.Service{Name: "svc", Namespace: "ns", Scale: 3, Status: types.ServiceStatusRunning}))

	var s types.Service
	err := store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &s, func() error {
		s.Scale = 99 // mutated, but then we abort — must NOT persist
		return ErrSkipUpdate
	})
	require.NoError(t, err, "ErrSkipUpdate must surface as a nil error")

	var got types.Service
	require.NoError(t, store.Get(ctx, types.ResourceTypeService, "ns", "svc", &got))
	assert.Equal(t, 3, got.Scale, "ErrSkipUpdate must leave the stored value unchanged")
}

// forceConflictOnce returns a mutate wrapper that, the first time it runs,
// commits a competing write to the same key from a separate transaction. The
// enclosing UpdateFunc attempt is then guaranteed to fail with ErrConflict and
// retry, which is the only way to exercise the retry path deterministically.
func forceConflictOnce(t *testing.T, s *BadgerStore, competing *types.Service) func() {
	t.Helper()
	var once sync.Once
	return func() {
		once.Do(func() {
			require.NoError(t, s.Update(context.Background(), types.ResourceTypeService,
				competing.Namespace, competing.Name, competing))
		})
	}
}

// TestUpdateFunc_NoStaleTargetOnRetry pins the contract that each retry decodes
// into a CLEAN target. json.Unmarshal never zeroes its destination, so without
// an explicit reset the mutation of a rejected attempt survives the re-read and
// is committed — silent corruption on the conflict path (RFC #129 primitive).
func TestUpdateFunc_NoStaleTargetOnRetry(t *testing.T) {
	// Scalar `omitempty` field: absent from the competing writer's JSON, so a
	// non-zeroed target keeps whatever attempt 1 put there.
	t.Run("scalar field", func(t *testing.T) {
		store, _, cleanup := setupTestStore(t)
		defer cleanup()
		ctx := context.Background()

		require.NoError(t, store.Create(ctx, types.ResourceTypeService, "ns", "svc",
			&types.Service{Name: "svc", Namespace: "ns", Scale: 1}))

		interfere := forceConflictOnce(t, store, &types.Service{
			Name: "svc", Namespace: "ns", Scale: 1, Status: types.ServiceStatusRunning,
		})

		var fresh types.Service
		attempt := 0
		require.NoError(t, store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &fresh, func() error {
			attempt++
			if attempt == 1 {
				fresh.StatusMessage = "rejected attempt"
				interfere()
			}
			fresh.Scale = 2
			return nil
		}))
		require.Equal(t, 2, attempt, "the competing write must have forced exactly one retry")

		var got types.Service
		require.NoError(t, store.Get(ctx, types.ResourceTypeService, "ns", "svc", &got))
		assert.Equal(t, 2, got.Scale)
		assert.Empty(t, got.StatusMessage, "a rejected attempt's write must not reach the store")
		assert.Empty(t, fresh.StatusMessage, "the retry must decode into a zeroed target")
	})

	// Slice of structs: encoding/json reuses the existing backing array
	// element-by-element, so a shorter re-read decodes element N over element 0's
	// slot and inherits every field the new element omits.
	t.Run("slice element aliasing", func(t *testing.T) {
		store, _, cleanup := setupTestStore(t)
		defer cleanup()
		ctx := context.Background()

		require.NoError(t, store.Create(ctx, types.ResourceTypeService, "ns", "svc",
			&types.Service{Name: "svc", Namespace: "ns", Scale: 2, Instances: []types.Instance{
				{ID: "a", FailureReason: "OOMKilled"},
				{ID: "b"},
			}}))

		// The competing writer leaves one instance, and it is NOT the one that
		// occupied slot 0.
		interfere := forceConflictOnce(t, store, &types.Service{
			Name: "svc", Namespace: "ns", Scale: 2, Instances: []types.Instance{{ID: "b"}},
		})

		var fresh types.Service
		attempt := 0
		require.NoError(t, store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &fresh, func() error {
			attempt++
			if attempt == 1 {
				fresh.Instances = append(fresh.Instances, types.Instance{ID: "c"})
				interfere()
			}
			fresh.Scale = 3
			return nil
		}))
		require.Equal(t, 2, attempt)

		var got types.Service
		require.NoError(t, store.Get(ctx, types.ResourceTypeService, "ns", "svc", &got))
		require.Len(t, got.Instances, 1)
		assert.Equal(t, "b", got.Instances[0].ID)
		assert.Empty(t, got.Instances[0].FailureReason,
			"instance b must not inherit a's failure reason from the discarded attempt's slot")
	})

	// Maps decode by merging into a non-nil destination, so keys the re-read
	// does not mention survive from the rejected attempt.
	t.Run("map merge", func(t *testing.T) {
		store, _, cleanup := setupTestStore(t)
		defer cleanup()
		ctx := context.Background()

		require.NoError(t, store.Create(ctx, types.ResourceTypeService, "ns", "svc",
			&types.Service{Name: "svc", Namespace: "ns", Scale: 1, Env: map[string]string{"KEEP": "1"}}))

		interfere := forceConflictOnce(t, store, &types.Service{
			Name: "svc", Namespace: "ns", Scale: 1, Env: map[string]string{"KEEP": "1"},
		})

		var fresh types.Service
		attempt := 0
		require.NoError(t, store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &fresh, func() error {
			attempt++
			if attempt == 1 {
				fresh.Env["LEAKED"] = "yes"
				interfere()
			}
			fresh.Scale = 2
			return nil
		}))
		require.Equal(t, 2, attempt)

		var got types.Service
		require.NoError(t, store.Get(ctx, types.ResourceTypeService, "ns", "svc", &got))
		assert.NotContains(t, got.Env, "LEAKED", "a rejected attempt's map key must not survive the retry")
		assert.Equal(t, "1", got.Env["KEEP"])
	})
}

// TestUpdateFunc_ReplacesPrefilledTarget holds every Store implementation to the
// same contract — the target is replaced by the stored value, never merged into.
// The retry path in BadgerStore is the case that corrupts data, but callers that
// reuse one target across calls would hit the same merge in the other stores.
func TestUpdateFunc_ReplacesPrefilledTarget(t *testing.T) {
	badger, _, cleanup := setupTestStore(t)
	defer cleanup()

	memory, test := NewMemoryStore(), NewTestStore()
	require.NoError(t, memory.Open(t.TempDir()))
	require.NoError(t, test.Open(t.TempDir()))

	stores := map[string]Store{
		"badger": badger, // already opened by setupTestStore
		"memory": memory,
		"test":   test,
	}
	for name, s := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			require.NoError(t, s.Create(ctx, types.ResourceTypeService, "ns", "prefill",
				&types.Service{Name: "prefill", Namespace: "ns", Scale: 1}))

			// Carries state no stored record has.
			fresh := types.Service{
				StatusMessage: "stale",
				Env:           map[string]string{"STALE": "1"},
				Instances:     []types.Instance{{ID: "ghost"}},
			}
			require.NoError(t, s.UpdateFunc(ctx, types.ResourceTypeService, "ns", "prefill", &fresh, func() error {
				fresh.Scale = 2
				return nil
			}))

			assert.Empty(t, fresh.StatusMessage, "prefilled fields must be replaced by the read, not merged")
			assert.Empty(t, fresh.Env)
			assert.Empty(t, fresh.Instances)
		})
	}
}
