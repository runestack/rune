package store

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Get decodes into the caller's value rather than replacing it, which is
// the opposite of UpdateFunc. Both take an interface{} target and look
// identical at the call site, so the difference is pinned here as well as
// documented: a caller that hoists a var out of a loop as a pure-looking
// optimization starts merging silently.
//
// If Get is ever changed to reset its target, this test should fail and be
// rewritten — deliberately. That is the point of it.
func TestGet_DecodesIntoTargetRatherThanReplacing(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, store.Create(ctx, types.ResourceTypeService, "ns", "svc",
		&types.Service{Name: "svc", Namespace: "ns", Scale: 1}))

	reused := types.Service{
		StatusMessage: "left over from a previous read",
		Env:           map[string]string{"STALE": "1"},
	}
	require.NoError(t, store.Get(ctx, types.ResourceTypeService, "ns", "svc", &reused))

	assert.Equal(t, "svc", reused.Name, "the stored row is read in")
	assert.Equal(t, "left over from a previous read", reused.StatusMessage,
		"an omitempty scalar the stored row omits SURVIVES — Get merges, it does not replace")
	assert.Equal(t, "1", reused.Env["STALE"], "and a non-nil map merges rather than being replaced")

	// The documented remedy: a fresh value.
	var fresh types.Service
	require.NoError(t, store.Get(ctx, types.ResourceTypeService, "ns", "svc", &fresh))
	assert.Empty(t, fresh.StatusMessage)
	assert.Empty(t, fresh.Env)
}

// UpdateFunc is the other half of the contrast: it zeroes the target, so a
// reused value is replaced rather than merged.
func TestUpdateFunc_ReplacesWhereGetMerges(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, store.Create(ctx, types.ResourceTypeService, "ns", "svc",
		&types.Service{Name: "svc", Namespace: "ns", Scale: 1}))

	reused := types.Service{
		StatusMessage: "left over from a previous read",
		Env:           map[string]string{"STALE": "1"},
	}
	require.NoError(t, store.UpdateFunc(ctx, types.ResourceTypeService, "ns", "svc", &reused,
		func() error { return nil }))

	assert.Equal(t, "svc", reused.Name)
	assert.Empty(t, reused.StatusMessage, "UpdateFunc zeroes the target where Get does not")
	assert.Empty(t, reused.Env)
}
