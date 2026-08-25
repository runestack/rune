package gpu_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/gpu"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gi48 = 48 << 30
	gi24 = 24 << 30
)

func newAdmitter(t *testing.T, devices ...types.GPUDevice) (*gpu.Admitter, *types.Node, store.Store) {
	t.Helper()
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(t, repos.NewNodeLedgerRepo(st).EnsureExists(context.Background(), "node-1"))
	probed := nowPtr()
	return gpu.NewAdmitter(st), &types.Node{
		ID: "node-1", Address: "127.0.0.1",
		Devices: devices, DevicesProbedAt: probed,
	}, st
}

func dev(uuid string, vram int64) types.GPUDevice {
	return types.GPUDevice{UUID: uuid, Vendor: "nvidia", Product: "NVIDIA L40S", VRAMBytes: vram}
}

func ledgerOf(t *testing.T, st store.Store) *types.NodeDeviceLedger {
	t.Helper()
	l, err := repos.NewNodeLedgerRepo(st).Get(context.Background(), "node-1")
	require.NoError(t, err)
	return l
}

func req(ns, svc, inst string, g types.GPURequest) gpu.Request {
	return gpu.Request{NodeID: "node-1", Namespace: ns, ServiceName: svc, InstanceID: inst, GPU: g}
}

// THE property the design rests on. The reconcile workqueue is exclusive
// per SERVICE key, not per device, so several workers can decide about
// one card at once. If capacity were checked before the write rather than
// inside it, each would read the same free space and each would admit.
//
// Eight concurrent whole-device claims against one card: exactly one wins.
func TestReserve_ConcurrentWholeDeviceClaimsAdmitExactlyOne(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))

	const workers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		refused  int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := adm.Reserve(context.Background(), node,
				req("prod", fmt.Sprintf("svc-%d", i), fmt.Sprintf("inst-%d", i), types.GPURequest{}))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				accepted++
				return
			}
			require.Equal(t, types.GPUReasonNoCapacity, gpu.ReasonOf(err), "refusals must be capacity refusals: %v", err)
			refused++
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 1, accepted, "a whole device can be claimed exactly once")
	assert.Equal(t, workers-1, refused)
	assert.Len(t, ledgerOf(t, st).Reservations, 1, "the ledger must agree with what was admitted")
}

// The same property for shared claims, where a lost update does its
// damage in the opposite direction and a naive assertion misses it.
//
// Under a read-then-write the ledger UNDER-counts: the last writer's
// whole-object write drops the rows it never saw. So "requested bytes
// stay under capacity" passes trivially while eight engines that were all
// told yes are running against two recorded rows — and the next admission
// overcommits against a ledger that has forgotten them.
//
// The property that actually holds is the one to assert: every Reserve
// that RETURNED SUCCESS is recorded. Verified by mutation — this fails
// against a read-then-write implementation, and the byte-sum assertion
// alone does not.
func TestReserve_EveryAdmittedClaimIsRecorded(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))

	const workers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted []string
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("inst-%d", i)
			if _, err := adm.Reserve(context.Background(), node,
				req("prod", fmt.Sprintf("svc-%d", i), id, types.GPURequest{VRAM: "10Gi"})); err == nil {
				mu.Lock()
				accepted = append(accepted, id)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	require.NotEmpty(t, accepted, "some claims should have succeeded")

	l := ledgerOf(t, st)
	recorded := map[string]bool{}
	for _, r := range l.Reservations {
		recorded[r.InstanceID] = true
	}
	for _, id := range accepted {
		assert.True(t, recorded[id],
			"%s was told its reservation succeeded but the ledger has no row for it — "+
				"a lost update means later admissions will overcommit against bytes it is using", id)
	}
	assert.Len(t, l.Reservations, len(accepted), "no extra rows either")

	// And the invariant itself still holds.
	total := l.RequestedBytes("GPU-1")
	capacity := int64(gi48) - l.ReservedBytes("GPU-1")
	assert.LessOrEqual(t, total, capacity,
		"admitted %d bytes against %d usable", total, capacity)
}

func TestReserve_WholeDeviceExcludesEveryoneElse(t *testing.T) {
	adm, node, _ := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	_, err := adm.Reserve(ctx, node, req("prod", "vllm", "i-1", types.GPURequest{}))
	require.NoError(t, err)

	// Even a tiny share cannot join a whole-device holder.
	_, err = adm.Reserve(ctx, node, req("prod", "tei", "i-2", types.GPURequest{VRAM: "1Gi"}))
	require.Error(t, err)
	assert.Equal(t, types.GPUReasonNoCapacity, gpu.ReasonOf(err))
}

func TestReserve_SharedDeviceAdmitsUpToCapacity(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	for i, want := range []string{"20Gi", "20Gi"} {
		_, err := adm.Reserve(ctx, node, req("prod", fmt.Sprintf("m%d", i), fmt.Sprintf("i-%d", i),
			types.GPURequest{VRAM: want}))
		require.NoError(t, err, "%s should fit", want)
	}
	// 40Gi requested + reserve leaves under 8Gi on a 48Gi card.
	_, err := adm.Reserve(ctx, node, req("prod", "m3", "i-3", types.GPURequest{VRAM: "8Gi"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no GPU with 8Gi free VRAM")
	assert.Contains(t, err.Error(), "prod/m0", "the refusal must name who holds the device")

	assert.Len(t, ledgerOf(t, st).Reservations, 2)
}

// A whole-device request records VRAMBytes: 0 with WholeDevice: true.
// Those two together are the encoding of "the whole card"; neither field
// means anything alone, which is why neither may be omitted.
func TestReserve_WholeDeviceRowShape(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	_, err := adm.Reserve(context.Background(), node, req("prod", "vllm", "i-1", types.GPURequest{}))
	require.NoError(t, err)

	r := ledgerOf(t, st).Reservations[0]
	assert.True(t, r.WholeDevice)
	assert.EqualValues(t, 0, r.VRAMBytes)
	assert.Equal(t, types.GPUResHolderInstance, r.Holder)
	assert.False(t, r.CreatedAt.IsZero(), "CreatedAt is what keeps the reclaim sweep off a mid-flight row")
}

// Release happens on terminal status, not on record deletion — a Failed
// instance is kept as its own tombstone for up to an hour, so releasing
// on deletion would let a crash-looping engine hold VRAM for that hour
// and block its own replacement.
func TestRelease_FreesCapacityForAReplacement(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	_, err := adm.Reserve(ctx, node, req("prod", "vllm", "i-1", types.GPURequest{}))
	require.NoError(t, err)

	_, err = adm.Reserve(ctx, node, req("prod", "vllm", "i-2", types.GPURequest{}))
	require.Error(t, err, "the card is held")

	require.NoError(t, adm.Release(ctx, "node-1", "i-1"))
	assert.Empty(t, ledgerOf(t, st).Reservations)

	_, err = adm.Reserve(ctx, node, req("prod", "vllm", "i-2", types.GPURequest{}))
	require.NoError(t, err, "the replacement must fit once its predecessor released")
}

// An idle hold has no instance by design, and Release must not take it —
// it is the whole point of letting a model scale to zero and keep its
// VRAM. Release is driven by instance ID, and an idle row's is empty, so
// a Release that matched on ID alone would sweep every idle hold on the
// node the first time any instance went terminal.
func TestRelease_LeavesIdleHoldsAlone(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	idle := req("prod", "vllm", "", types.GPURequest{VRAM: "20Gi"})
	idle.Holder = types.GPUResHolderIdle
	_, err := adm.Reserve(ctx, node, idle)
	require.NoError(t, err)

	_, err = adm.Reserve(ctx, node, req("prod", "tei", "i-1", types.GPURequest{VRAM: "2Gi"}))
	require.NoError(t, err)
	require.Len(t, ledgerOf(t, st).Reservations, 2)

	// Releasing the instance must take its row and ONLY its row.
	require.NoError(t, adm.Release(ctx, "node-1", "i-1"))

	remaining := ledgerOf(t, st).Reservations
	require.Len(t, remaining, 1, "the idle hold must survive an unrelated instance release")
	assert.Equal(t, types.GPUResHolderIdle, remaining[0].Holder)
	assert.Equal(t, "vllm", remaining[0].ServiceName)
}

// A reservation whose instance record does not exist yet is byte-identical
// to an idle hold except for Holder. Releasing by the empty ID must not
// take either — that would make a crashed reserve destroy a live feature.
func TestRelease_EmptyInstanceIDReleasesNothing(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	idle := req("prod", "vllm", "", types.GPURequest{VRAM: "20Gi"})
	idle.Holder = types.GPUResHolderIdle
	_, err := adm.Reserve(ctx, node, idle)
	require.NoError(t, err)
	_, err = adm.Reserve(ctx, node, req("prod", "tei", "", types.GPURequest{VRAM: "2Gi"}))
	require.NoError(t, err)

	require.Error(t, adm.Release(ctx, "node-1", ""), "releasing by an empty ID must be refused, not applied")
	assert.Len(t, ledgerOf(t, st).Reservations, 2)
}

func TestReleaseService_TakesIdleHoldsToo(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	idle := req("prod", "vllm", "", types.GPURequest{VRAM: "20Gi"})
	idle.Holder = types.GPUResHolderIdle
	_, err := adm.Reserve(ctx, node, idle)
	require.NoError(t, err)

	require.NoError(t, adm.ReleaseService(ctx, "node-1", "prod", "vllm"))
	assert.Empty(t, ledgerOf(t, st).Reservations,
		"a deleted service leaves nothing to reclaim its idle hold later")
}

// A reservation is written before its instance record exists. Binding
// fills the ID in, which is what makes the row reclaimable by instance.
func TestBindInstance(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	p, err := adm.Reserve(ctx, node, req("prod", "vllm", "", types.GPURequest{}))
	require.NoError(t, err)
	require.Len(t, p.DeviceUUIDs, 1)

	require.NoError(t, adm.BindInstance(ctx, "node-1", "prod", "vllm", "i-9", p.DeviceUUIDs))
	assert.Equal(t, "i-9", ledgerOf(t, st).Reservations[0].InstanceID)

	require.NoError(t, adm.Release(ctx, "node-1", "i-9"))
	assert.Empty(t, ledgerOf(t, st).Reservations)
}

// A no-op release must not write. The ledger is one hot key every GPU
// transition on the node contends for, and each write also appends an
// unpruned version-history row.
func TestRelease_UnknownInstanceWritesNothing(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()
	_, err := adm.Reserve(ctx, node, req("prod", "vllm", "i-1", types.GPURequest{}))
	require.NoError(t, err)

	before := ledgerOf(t, st).UpdatedAt
	require.NoError(t, adm.Release(ctx, "node-1", "not-a-real-instance"))
	assert.Equal(t, before, ledgerOf(t, st).UpdatedAt, "a release that frees nothing must not write")
}

// Unprobed inventory is not a statement that the node has no GPUs. The
// agent starts after the control plane, so every restart has a window
// where the answer is unknown — refusing there would blame the driver and
// write Failed tombstones on every upgrade.
func TestCanFit_UnprobedInventoryIsRetryableNotACapacityRefusal(t *testing.T) {
	node := &types.Node{ID: "node-1", Address: "127.0.0.1"} // DevicesProbedAt nil
	_, err := gpu.CanFit(node, &types.NodeDeviceLedger{}, "prod", types.GPURequest{})
	require.Error(t, err)
	assert.NotEqual(t, types.GPUReasonNoCapacity, gpu.ReasonOf(err),
		"never NoGpuCapacity — that is a claim about capacity the node has not made")
	assert.Contains(t, err.Error(), "waiting for this node's device inventory")
}

// CanFit is advisory and must not mutate.
func TestCanFit_NeverWrites(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()
	_, err := adm.Reserve(ctx, node, req("prod", "vllm", "i-1", types.GPURequest{VRAM: "20Gi"}))
	require.NoError(t, err)

	before := ledgerOf(t, st)
	for i := 0; i < 3; i++ {
		_, _ = gpu.CanFit(node, before, "prod", types.GPURequest{VRAM: "20Gi"})
	}
	after := ledgerOf(t, st)
	assert.Equal(t, len(before.Reservations), len(after.Reservations))
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt)
}

func nowPtr() *time.Time {
	t := time.Now()
	return &t
}

// Binding must take exactly ONE row per device. A scale-2 service sharing
// one card has two unbound rows for the same (namespace, service,
// device); stamping both with one instance's ID means releasing that
// instance drops the other instance's reservation too, and the card reads
// as free while an engine is still holding memory on it.
func TestBindInstance_TakesOneRowPerDevice(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, err := adm.Reserve(ctx, node, req("prod", "llm", "", types.GPURequest{VRAM: "20Gi"}))
		require.NoError(t, err)
	}

	require.NoError(t, adm.BindInstance(ctx, "node-1", "prod", "llm", "i-1", []string{"GPU-1"}))
	require.NoError(t, adm.BindInstance(ctx, "node-1", "prod", "llm", "i-2", []string{"GPU-1"}))

	got := map[string]int{}
	for _, r := range ledgerOf(t, st).Reservations {
		got[r.InstanceID]++
	}
	assert.Equal(t, map[string]int{"i-1": 1, "i-2": 1}, got,
		"each bind must claim one row, not every matching row")

	// And releasing one leaves the other's memory accounted for.
	require.NoError(t, adm.Release(ctx, "node-1", "i-1"))
	l := ledgerOf(t, st)
	require.Len(t, l.Reservations, 1)
	assert.Equal(t, "i-2", l.Reservations[0].InstanceID)
	assert.EqualValues(t, 20<<30, l.RequestedBytes("GPU-1"),
		"the surviving instance's bytes must still be counted")
}

// Binding a device with no unbound row is an error, not silence: the
// caller believes it holds a reservation there.
func TestBindInstance_ErrorsWhenNothingToBind(t *testing.T) {
	adm, node, _ := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()
	_, err := adm.Reserve(ctx, node, req("prod", "llm", "", types.GPURequest{VRAM: "20Gi"}))
	require.NoError(t, err)

	require.NoError(t, adm.BindInstance(ctx, "node-1", "prod", "llm", "i-1", []string{"GPU-1"}))
	err = adm.BindInstance(ctx, "node-1", "prod", "llm", "i-2", []string{"GPU-1"})
	require.Error(t, err, "there is no second unbound row; silence would let the caller believe otherwise")
	assert.Contains(t, err.Error(), "no unbound reservation")
}

// A release and the reservation that replaces it must be ONE write. Two
// writes open a window where the freed bytes are visible to a concurrent
// admission that should not get them, on the one key every GPU transition
// on the node contends for.
func TestReserveReplacing_IsASingleTransition(t *testing.T) {
	adm, node, st := newAdmitter(t, dev("GPU-1", gi48))
	ctx := context.Background()

	_, err := adm.Reserve(ctx, node, req("prod", "vllm", "old", types.GPURequest{}))
	require.NoError(t, err)
	before := ledgerOf(t, st).UpdatedAt

	// The card is held whole, so this can only succeed if the release
	// happens in the same mutate.
	p, err := adm.ReserveReplacing(ctx, node, "old", req("prod", "vllm", "new", types.GPURequest{}))
	require.NoError(t, err, "the replacement must fit against the retired instance's freed device")
	assert.Equal(t, []string{"GPU-1"}, p.DeviceUUIDs)

	l := ledgerOf(t, st)
	require.Len(t, l.Reservations, 1)
	assert.Equal(t, "new", l.Reservations[0].InstanceID)
	assert.True(t, l.UpdatedAt.After(before))
}

// The ledger row is created by the agent when it writes inventory, but
// that write is best-effort. An admission against a node whose row never
// landed must still get an admission answer, not a raw store error.
func TestReserve_CreatesTheLedgerWhenAbsent(t *testing.T) {
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })

	probed := nowPtr()
	node := &types.Node{ID: "node-1", Address: "127.0.0.1",
		Devices: []types.GPUDevice{dev("GPU-1", gi48)}, DevicesProbedAt: probed}

	// No EnsureExists call: the row does not exist.
	adm := gpu.NewAdmitter(st)
	_, err := adm.Reserve(context.Background(), node, req("prod", "vllm", "i-1", types.GPURequest{}))
	require.NoError(t, err, "the first reservation on a box must not fail for want of a row")
	assert.Len(t, ledgerOf(t, st).Reservations, 1)
}

// A ledger keyed to one node checked against another node's devices would
// record a claim against capacity that was never examined.
func TestReserve_RefusesMismatchedNodeAndLedger(t *testing.T) {
	adm, node, _ := newAdmitter(t, dev("GPU-1", gi48))
	r := req("prod", "vllm", "i-1", types.GPURequest{})
	r.NodeID = "some-other-node"

	_, err := adm.Reserve(context.Background(), node, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node mismatch")
}

// The Holder filter in Release, pinned on the only state where it does
// any work.
//
// It looks redundant: Release matches on instance ID, and an idle hold
// normally has none, so the ID comparison alone already spares it. But
// nothing in the type forbids an idle hold from carrying an instance ID —
// a model that was serving and then scaled to zero is exactly that shape
// — and there the ID comparison matches, leaving Holder as the only thing
// between RUNE-310's feature and a Release that destroys it. Seeded
// directly, because Reserve's callers cannot currently produce this state
// and a test that cannot reach a state cannot guard it.
func TestRelease_IdleHoldSurvivesEvenWithAnInstanceID(t *testing.T) {
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })

	seeded := &types.NodeDeviceLedger{
		NodeID: "node-1",
		Reservations: []types.GPURes{
			{DeviceUUID: "GPU-1", Namespace: "prod", ServiceName: "vllm",
				InstanceID: "i-1", VRAMBytes: 20 << 30,
				Holder: types.GPUResHolderIdle, CreatedAt: time.Now()},
			{DeviceUUID: "GPU-1", Namespace: "prod", ServiceName: "tei",
				InstanceID: "i-1", VRAMBytes: 2 << 30,
				Holder: types.GPUResHolderInstance, CreatedAt: time.Now()},
		},
	}
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeNodeLedger, "", "node-1", seeded))

	require.NoError(t, gpu.NewAdmitter(st).Release(context.Background(), "node-1", "i-1"))

	l, err := repos.NewNodeLedgerRepo(st).Get(context.Background(), "node-1")
	require.NoError(t, err)
	require.Len(t, l.Reservations, 1, "the idle hold must survive a release of the same instance ID")
	assert.Equal(t, types.GPUResHolderIdle, l.Reservations[0].Holder)
	assert.Equal(t, "vllm", l.Reservations[0].ServiceName)
}
