package repos_test

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newNodeRepo(t *testing.T) (*repos.NodeRepo, store.Store) {
	t.Helper()
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	return repos.NewNodeRepo(st), st
}

func TestNodeRepo_UpsertCreatesThenReplaces(t *testing.T) {
	repo, _ := newNodeRepo(t)
	ctx := context.Background()

	probedAt := time.Now().Add(-time.Hour).UTC()
	first := &types.Node{
		ID:              "node-8f6a12cd",
		Address:         "127.0.0.1",
		Devices:         []types.GPUDevice{{UUID: "GPU-1", Vendor: "nvidia", VRAMBytes: 48 << 30}},
		DevicesProbedAt: &probedAt,
	}
	require.NoError(t, repo.Upsert(ctx, first))

	got, err := repo.Get(ctx, "node-8f6a12cd")
	require.NoError(t, err)
	assert.Equal(t, "node-8f6a12cd", got.Name, "Name defaults to the ID")
	require.Len(t, got.Devices, 1)
	assert.Equal(t, "GPU-1", got.Devices[0].UUID)
	assert.False(t, got.CreatedAt.IsZero())
	createdAt := got.CreatedAt

	// A second probe replaces the record wholesale but keeps CreatedAt.
	second := &types.Node{ID: "node-8f6a12cd", Address: "127.0.0.1", DeviceProbeError: "nvidia-smi not found on PATH"}
	require.NoError(t, repo.Upsert(ctx, second))

	got, err = repo.Get(ctx, "node-8f6a12cd")
	require.NoError(t, err)
	assert.Equal(t, "nvidia-smi not found on PATH", got.DeviceProbeError)
	assert.WithinDuration(t, createdAt, got.CreatedAt, time.Millisecond, "CreatedAt survives a re-probe")

	nodes, err := repo.List(ctx)
	require.NoError(t, err)
	assert.Len(t, nodes, 1)
}

// The record's `omitempty` fields must not survive a write that clears
// them. BadgerStore.UpdateFunc re-reads into an un-zeroed target inside
// its retry loop, so an absent `omitempty` field keeps whatever the
// target already held. NodeRepo.Upsert avoids it by writing the whole
// object through Update instead of a read-modify-write — this test is
// what stops a future refactor to UpdateFunc from silently
// reintroducing it.
func TestNodeRepo_UpsertClearsOmitemptyFields(t *testing.T) {
	repo, _ := newNodeRepo(t)
	ctx := context.Background()

	probedAt := time.Now().UTC()
	require.NoError(t, repo.Upsert(ctx, &types.Node{
		ID:               "node-1",
		Address:          "127.0.0.1",
		Labels:           map[string]string{"role": "edge"},
		StatusReason:     "HeartbeatTimeout",
		StatusMessage:    "no heartbeat for 60s",
		Devices:          []types.GPUDevice{{UUID: "GPU-1"}},
		DevicesProbedAt:  &probedAt,
		DeviceProbeError: "permission denied",
	}))

	// Now write a record with every one of those fields at its zero
	// value — the GPU went away, the probe succeeded, the labels were
	// dropped.
	require.NoError(t, repo.Upsert(ctx, &types.Node{ID: "node-1", Address: "127.0.0.1"}))

	got, err := repo.Get(ctx, "node-1")
	require.NoError(t, err)
	assert.Nil(t, got.Labels, "Labels must not survive a write that clears them")
	assert.Empty(t, got.StatusReason)
	assert.Empty(t, got.StatusMessage)
	assert.Empty(t, got.Devices, "a device list must not survive a write that clears it")
	assert.Nil(t, got.DevicesProbedAt)
	assert.Empty(t, got.DeviceProbeError)
}

// Validate() is on the write path, not routed around it.
func TestNodeRepo_UpsertRejectsInvalid(t *testing.T) {
	repo, _ := newNodeRepo(t)
	ctx := context.Background()

	assert.Error(t, repo.Upsert(ctx, &types.Node{ID: "", Address: "127.0.0.1"}), "an empty ID is rejected")
	assert.Error(t, repo.Upsert(ctx, &types.Node{ID: "node-1"}), "an empty Address is rejected")
	assert.Error(t, repo.Upsert(ctx, nil))
}

// The hazard that used to make this repo's whole-object write necessary,
// pinned on types.Node so it stays closed. UpdateFunc once re-read the
// stored row into the SAME target on every attempt without zeroing it,
// and encoding/json leaves an absent field untouched — so a target
// already carrying a device list kept it even when the stored row had
// none, and committed it.
//
// UpdateFunc now zeroes the target before each read. types.Node is a
// worthwhile canary because it carries six `omitempty` fields across
// three of the four leaking shapes: scalars, a map, a pointer and a
// slice.
func TestUpdateFuncZeroesTargetBeforeReadingNode(t *testing.T) {
	repo, st := newNodeRepo(t)
	ctx := context.Background()

	// Stored state: a node with no devices, no probe error, no labels.
	require.NoError(t, repo.Upsert(ctx, &types.Node{ID: "node-1", Address: "127.0.0.1"}))

	// A caller whose target already holds a previous read — exactly what
	// the retry loop used to do to itself.
	probedAt := time.Now()
	target := &types.Node{
		Devices:          []types.GPUDevice{{UUID: "GPU-stale"}},
		DeviceProbeError: "stale probe error",
		Labels:           map[string]string{"stale": "label"},
		DevicesProbedAt:  &probedAt,
		StatusReason:     "StaleReason",
	}
	require.NoError(t, st.UpdateFunc(ctx, types.ResourceTypeNode, "", "node-1", target, func() error {
		return nil
	}))

	assert.Empty(t, target.Devices, "a slice absent from the stored row must not survive the read")
	assert.Empty(t, target.DeviceProbeError, "nor a scalar")
	assert.Nil(t, target.Labels, "nor a map, which would otherwise merge")
	assert.Nil(t, target.DevicesProbedAt, "nor a pointer, which would otherwise decode through")
	assert.Empty(t, target.StatusReason)
	assert.Equal(t, "node-1", target.ID, "and the stored row is still read into it")

	// Nothing stale reached the store.
	got, err := repo.Get(ctx, "node-1")
	require.NoError(t, err)
	assert.Empty(t, got.Devices)
	assert.Empty(t, got.DeviceProbeError)
	assert.Nil(t, got.Labels)
}
