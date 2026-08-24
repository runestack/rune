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
// them. This is the hazard RUNE-301 D-24 documents for the P2 ledger:
// BadgerStore.UpdateFunc re-reads into an un-zeroed target inside its
// retry loop, so an absent `omitempty` field keeps whatever the target
// already held. NodeRepo.Upsert avoids it by writing the whole object
// through Update instead of a read-modify-write — this test is what
// stops a future refactor to UpdateFunc from silently reintroducing it.
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

// The hazard NodeRepo.Upsert is avoiding, demonstrated on types.Node so
// the choice in node.go is backed by a reproduction rather than a claim.
// UpdateFunc re-reads the stored row into the SAME target on every
// attempt without zeroing it, and encoding/json leaves an absent field
// untouched — so a target that already carries a device list keeps it
// even when the stored row has none.
func TestUpdateFuncKeepsStaleOmitemptyFieldsOnNode(t *testing.T) {
	repo, st := newNodeRepo(t)
	ctx := context.Background()

	// Stored state: a node with no devices and no probe error.
	require.NoError(t, repo.Upsert(ctx, &types.Node{ID: "node-1", Address: "127.0.0.1"}))

	// A caller that reuses a target already holding a previous read —
	// exactly what UpdateFunc's retry loop does to itself.
	target := &types.Node{
		Devices:          []types.GPUDevice{{UUID: "GPU-stale"}},
		DeviceProbeError: "stale probe error",
	}
	require.NoError(t, st.UpdateFunc(ctx, types.ResourceTypeNode, "", "node-1", target, func() error {
		return nil
	}))

	assert.Equal(t, "GPU-stale", target.Devices[0].UUID,
		"the un-zeroed target keeps a device list the stored row does not have")
	assert.Equal(t, "stale probe error", target.DeviceProbeError,
		"and keeps a probe error the stored row does not have")

	// And the bad value is now committed, which is why Upsert does not
	// go through this path.
	got, err := repo.Get(ctx, "node-1")
	require.NoError(t, err)
	assert.Len(t, got.Devices, 1, "the stale device list was written back to the store")
}
