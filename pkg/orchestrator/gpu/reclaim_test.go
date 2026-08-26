package gpu_test

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/orchestrator/gpu"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AdoptAssignment carries its own idempotency, and it has to: the caller
// works out what is missing from a snapshot read OUTSIDE this
// transaction, so by the time it commits the gap may already be closed.
// A second row for the same card double-counts its VRAM and refuses the
// next service that should have fitted.
//
// The reconciler's own "is it already covered" check would hide this, so
// this test goes at the admitter directly.
func TestAdoptAssignment_SecondCallAddsNoSecondRow(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()
	r := req("default", "vllm", "inst-1", types.GPURequest{VRAM: "20Gi"})

	require.NoError(t, adm.AdoptAssignment(ctx, node, r, []string{"GPU-1"}))
	require.NoError(t, adm.AdoptAssignment(ctx, node, r, []string{"GPU-1"}))

	rows := ledgerOf(t, st).Reservations
	require.Len(t, rows, 1)
	assert.EqualValues(t, 20<<30, ledgerOf(t, st).RequestedBytes("GPU-1"))
}

// A tensor-parallel instance holds several cards, and the ledger lost
// all of them. Adopting one and calling it done leaves the rest reading
// free while the engine has memory on them.
func TestAdoptAssignment_TakesEveryDeviceTheInstanceHolds(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48), dev("GPU-2", gi48))
	ctx := context.Background()

	require.NoError(t, adm.AdoptAssignment(ctx, node,
		req("default", "vllm", "inst-1", types.GPURequest{Count: 2}),
		[]string{"GPU-1", "GPU-2"}))

	rows := ledgerOf(t, st).Reservations
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"GPU-1", "GPU-2"},
		[]string{rows[0].DeviceUUID, rows[1].DeviceUUID})
	for _, r := range rows {
		assert.Equal(t, "inst-1", r.InstanceID)
		assert.True(t, r.WholeDevice)
	}
}

// A partly-covered assignment adopts only the gap. Re-taking the device
// it already holds would count that one twice.
func TestAdoptAssignment_TakesOnlyTheUncoveredDevices(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48), dev("GPU-2", gi48))
	ctx := context.Background()
	r := req("default", "vllm", "inst-1", types.GPURequest{Count: 2})

	require.NoError(t, adm.AdoptAssignment(ctx, node, r, []string{"GPU-1"}))
	require.NoError(t, adm.AdoptAssignment(ctx, node, r, []string{"GPU-1", "GPU-2"}))

	rows := ledgerOf(t, st).Reservations
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"GPU-1", "GPU-2"},
		[]string{rows[0].DeviceUUID, rows[1].DeviceUUID})
}

// The card an instance is running on has gone from the inventory. Taking
// a reservation anyway would claim capacity against hardware the node no
// longer reports — a number that can never be checked against anything.
func TestAdoptAssignment_RefusesADeviceTheNodeNoLongerReports(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	err := adm.AdoptAssignment(ctx, node,
		req("default", "vllm", "inst-1", types.GPURequest{}), []string{"GPU-9"})

	require.Error(t, err)
	assert.Equal(t, types.GPUReasonDeviceMissing, gpu.ReasonOf(err))
	assert.Empty(t, ledgerOf(t, st).Reservations)
}

// A device marked missing is the same situation with the row still
// present — the driver stopped reporting it, the record remembers it.
func TestAdoptAssignment_RefusesAMissingDevice(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	node.Devices[0].Missing = true
	ctx := context.Background()

	err := adm.AdoptAssignment(ctx, node,
		req("default", "vllm", "inst-1", types.GPURequest{}), []string{"GPU-1"})

	require.Error(t, err)
	assert.Equal(t, types.GPUReasonDeviceMissing, gpu.ReasonOf(err))
	assert.Empty(t, ledgerOf(t, st).Reservations)
}

// All-or-nothing across a multi-device assignment: one device that no
// longer fits must not leave the others half-claimed, because a partial
// adopt reads as a complete one on the next tick.
func TestAdoptAssignment_WritesNothingWhenOneDeviceIsRefused(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-A", gi48), dev("GPU-B", gi48))
	ctx := context.Background()

	// Whichever card the other service takes has to be the SECOND one
	// checked, or the refusal comes before anything could have been
	// written and the all-or-nothing claim is never exercised.
	_, err := adm.Reserve(ctx, node, req("other", "tei", "other-1", types.GPURequest{}))
	require.NoError(t, err)
	taken := ledgerOf(t, st).Reservations[0].DeviceUUID
	free := "GPU-B"
	if taken == "GPU-B" {
		free = "GPU-A"
	}

	err = adm.AdoptAssignment(ctx, node,
		req("default", "vllm", "inst-1", types.GPURequest{Count: 2}),
		[]string{free, taken})

	require.Error(t, err)
	assert.Equal(t, types.GPUReasonOverCommitted, gpu.ReasonOf(err))
	rows := ledgerOf(t, st).Reservations
	require.Len(t, rows, 1, "the device checked before the refusal must not be claimed")
	assert.Equal(t, taken, rows[0].DeviceUUID)
}

// A device named twice is one claim, not two. The check loop runs against
// the pre-append ledger, so a duplicate would pass it twice — and a
// restored or hand-edited record is exactly what this path reads.
func TestAdoptAssignment_RefusesToCountADeviceTwice(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	require.NoError(t, adm.AdoptAssignment(ctx, node,
		req("default", "vllm", "inst-1", types.GPURequest{VRAM: "10Gi"}),
		[]string{"GPU-1", "GPU-1"}))

	rows := ledgerOf(t, st).Reservations
	require.Len(t, rows, 1)
	assert.EqualValues(t, 10<<30, ledgerOf(t, st).RequestedBytes("GPU-1"))
}

// Reclaim is keyed on the ledger's own node. A row on another node's
// ledger is not this call's to free.
func TestReclaimOrphans_LeavesOtherNodesAlone(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()
	_, err := adm.Reserve(ctx, node, req("default", "vllm", "ghost", types.GPURequest{}))
	require.NoError(t, err)

	// A second node with an orphan of its own, so the call has something
	// it COULD have taken if it ignored the key it was given.
	require.NoError(t, repos.NewNodeLedgerRepo(st).EnsureExists(ctx, "node-2"))
	other := &types.Node{ID: "node-2", Devices: []types.GPUDevice{dev("GPU-9", gi48)}, DevicesProbedAt: nowPtr()}
	_, err = adm.Reserve(ctx, other, gpu.Request{
		NodeID: "node-2", Namespace: "default", ServiceName: "tei", InstanceID: "ghost-2",
	})
	require.NoError(t, err)

	dropped, err := adm.ReclaimOrphans(ctx, "node-2", map[string]bool{}, time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, dropped, 1, "the node it was asked about is swept")
	assert.Equal(t, "GPU-9", dropped[0].DeviceUUID)
	assert.Len(t, ledgerOf(t, st).Reservations, 1, "and the one it was not is untouched")
}

// A node with no ledger at all must not have one conjured for it.
// Creating a row for any node ID a caller names grows the orphan set from
// the other end.
func TestReclaimOrphans_AbsentLedgerIsNotCreated(t *testing.T) {
	adm, _, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	dropped, err := adm.ReclaimOrphans(ctx, "node-never-seen", map[string]bool{}, time.Now().UTC())
	require.NoError(t, err)
	assert.Empty(t, dropped)

	var ledger types.NodeDeviceLedger
	err = st.Get(ctx, types.ResourceTypeNodeLedger, "", "node-never-seen", &ledger)
	assert.Error(t, err, "reclaiming against an unknown node must not write a ledger for it")
}
