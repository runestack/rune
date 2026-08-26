package instance

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/gpu"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testNodeID = "node-gpu-1"

// gpuController builds a controller with a real store, a node record
// carrying the given devices, and GPU admission wired.
func gpuController(t *testing.T, devices ...types.GPUDevice) (context.Context, *Controller, store.Store) {
	t.Helper()
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	probed := time.Now()
	require.NoError(t, repos.NewNodeRepo(st).Upsert(ctx, &types.Node{
		ID: testNodeID, Address: "127.0.0.1",
		Devices: devices, DevicesProbedAt: &probed,
	}))
	require.NoError(t, repos.NewNodeLedgerRepo(st).EnsureExists(ctx, testNodeID))

	testRunner := runner.NewTestRunner()
	mgr := manager.NewTestRunnerManager(nil)
	mgr.SetDockerRunner(testRunner)
	mgr.SetProcessRunner(testRunner)

	c := NewController(st, mgr, log.NewTestLogger(),
		WithNodeID(testNodeID), WithGPUAdmitter(gpu.NewAdmitter(st)))
	return ctx, c, st
}

func gpuService(t *testing.T, st store.Store, name string, req *types.GPURequest) *types.Service {
	t.Helper()
	svc := &types.Service{
		ID: name + "-id", Name: name, Namespace: "default", Image: "vllm/vllm-openai",
		Scale: 1, Resources: types.Resources{GPU: req},
	}
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeService, "default", svc.ID, svc))
	return svc
}

func ledger(t *testing.T, st store.Store) *types.NodeDeviceLedger {
	t.Helper()
	l, err := repos.NewNodeLedgerRepo(st).Get(context.Background(), testNodeID)
	require.NoError(t, err)
	return l
}

func gpuDev(uuid string, vram int64) types.GPUDevice {
	return types.GPUDevice{UUID: uuid, Vendor: "nvidia", Product: "NVIDIA L40S", VRAMBytes: vram}
}

// Creating a GPU service takes a reservation and records the devices on
// the instance, keyed by the instance's own ID — the UUID is minted
// before any of this, so there is no window where the row has no owner.
func TestCreateInstance_ReservesAndRecordsDevices(t *testing.T) {
	ctx, c, st := gpuController(t, gpuDev("GPU-1", 48<<30))
	svc := gpuService(t, st, "vllm", &types.GPURequest{})

	inst, err := c.CreateInstance(ctx, svc, "vllm-0", 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"GPU-1"}, inst.GPUAssignments)

	rows := ledger(t, st).Reservations
	require.Len(t, rows, 1)
	assert.Equal(t, inst.ID, rows[0].InstanceID, "the reservation is owned from the moment it is written")
	assert.True(t, rows[0].WholeDevice)
	assert.Equal(t, types.GPUResHolderInstance, rows[0].Holder)
}

// A service with no gpu request must not touch the ledger at all — no
// row, and no read of a node record it does not need.
func TestCreateInstance_NoGPURequestTouchesNothing(t *testing.T) {
	ctx, c, st := gpuController(t, gpuDev("GPU-1", 48<<30))
	svc := gpuService(t, st, "plain", nil)

	inst, err := c.CreateInstance(ctx, svc, "plain-0", 0)
	require.NoError(t, err)
	assert.Empty(t, inst.GPUAssignments)
	assert.Empty(t, ledger(t, st).Reservations)
}

// Admission refuses before the record exists. An instance that cannot be
// placed must not leave a tombstone for a decision Rune already made.
func TestCreateInstance_RefusedAdmissionCreatesNoInstance(t *testing.T) {
	ctx, c, st := gpuController(t, gpuDev("GPU-1", 48<<30))

	first := gpuService(t, st, "vllm", &types.GPURequest{})
	_, err := c.CreateInstance(ctx, first, "vllm-0", 0)
	require.NoError(t, err)

	second := gpuService(t, st, "tei", &types.GPURequest{})
	_, err = c.CreateInstance(ctx, second, "tei-0", 0)
	require.Error(t, err, "the only card is held whole")
	assert.Equal(t, types.GPUReasonNoCapacity, gpu.ReasonOf(err))

	var instances []types.Instance
	require.NoError(t, st.List(ctx, types.ResourceTypeInstance, "default", &instances))
	assert.Len(t, instances, 1, "the refused service must not have left an instance record")
	assert.Len(t, ledger(t, st).Reservations, 1)
}

// The whole point of releasing on terminal status rather than on record
// deletion: a Failed instance is kept as its own tombstone for up to an
// hour, and holding its VRAM for that hour would block the replacement
// that fixes the crash loop.
func TestMarkFailed_ReleasesSoTheReplacementFits(t *testing.T) {
	ctx, c, st := gpuController(t, gpuDev("GPU-1", 48<<30))
	svc := gpuService(t, st, "vllm", &types.GPURequest{})

	inst, err := c.CreateInstance(ctx, svc, "vllm-0", 0)
	require.NoError(t, err)

	require.NoError(t, c.markInstanceFailedInPlace(ctx, inst, RestartReasonFailure))
	assert.Empty(t, ledger(t, st).Reservations, "the terminal status frees the card")

	// The tombstone is still there — this is release WITHOUT deletion.
	var stored types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, "default", inst.ID, &stored))
	assert.Equal(t, types.InstanceStatusFailed, stored.Status)

	// And the replacement fits.
	replacement, err := c.CreateInstance(ctx, svc, "vllm-0", 0)
	require.NoError(t, err, "a crash-looping engine must not block its own replacement")
	assert.Equal(t, []string{"GPU-1"}, replacement.GPUAssignments)
}

func TestStopInstance_Releases(t *testing.T) {
	ctx, c, st := gpuController(t, gpuDev("GPU-1", 48<<30))
	svc := gpuService(t, st, "vllm", &types.GPURequest{})

	inst, err := c.CreateInstance(ctx, svc, "vllm-0", 0)
	require.NoError(t, err)
	require.Len(t, ledger(t, st).Reservations, 1)

	require.NoError(t, c.StopInstance(ctx, inst))
	assert.Empty(t, ledger(t, st).Reservations)
}

// Releasing one instance must not free another's card.
func TestRelease_TakesOnlyItsOwnDevices(t *testing.T) {
	ctx, c, st := gpuController(t, gpuDev("GPU-1", 48<<30), gpuDev("GPU-2", 48<<30))
	a := gpuService(t, st, "vllm", &types.GPURequest{})
	b := gpuService(t, st, "tei", &types.GPURequest{})

	instA, err := c.CreateInstance(ctx, a, "vllm-0", 0)
	require.NoError(t, err)
	instB, err := c.CreateInstance(ctx, b, "tei-0", 0)
	require.NoError(t, err)
	require.Len(t, ledger(t, st).Reservations, 2)

	require.NoError(t, c.markInstanceFailedInPlace(ctx, instA, RestartReasonFailure))

	rows := ledger(t, st).Reservations
	require.Len(t, rows, 1)
	assert.Equal(t, instB.ID, rows[0].InstanceID)
	assert.Equal(t, instB.GPUAssignments, []string{rows[0].DeviceUUID})
}

// Shared devices: two models on one card, summed against its capacity.
func TestCreateInstance_SharedDeviceAccumulates(t *testing.T) {
	ctx, c, st := gpuController(t, gpuDev("GPU-1", 48<<30))
	for _, name := range []string{"m1", "m2"} {
		svc := gpuService(t, st, name, &types.GPURequest{VRAM: "20Gi"})
		_, err := c.CreateInstance(ctx, svc, name+"-0", 0)
		require.NoError(t, err, "%s should fit", name)
	}
	assert.EqualValues(t, 40<<30, ledger(t, st).RequestedBytes("GPU-1"))

	third := gpuService(t, st, "m3", &types.GPURequest{VRAM: "8Gi"})
	_, err := c.CreateInstance(ctx, third, "m3-0", 0)
	require.Error(t, err, "40Gi held plus the system reserve leaves under 8Gi on a 48Gi card")
}

// With no admitter wired — embedded use, and every install before this
// existed — a GPU service is created exactly as before, reserving
// nothing. The feature has to be absent, not merely inert.
func TestCreateInstance_NoAdmitterIsUnchanged(t *testing.T) {
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	testRunner := runner.NewTestRunner()
	mgr := manager.NewTestRunnerManager(nil)
	mgr.SetDockerRunner(testRunner)
	mgr.SetProcessRunner(testRunner)
	c := NewController(st, mgr, log.NewTestLogger(), WithNodeID(testNodeID))

	svc := gpuService(t, st, "vllm", &types.GPURequest{})
	inst, err := c.CreateInstance(ctx, svc, "vllm-0", 0)
	require.NoError(t, err, "no admitter must not turn a GPU service into a failure")
	assert.Empty(t, inst.GPUAssignments)
}

// Inventory that has not been probed yet is not "no GPUs". The agent
// starts after the control plane, so every restart passes through this
// window; refusing as a capacity problem would write hour-long tombstones
// on every upgrade.
func TestCreateInstance_UnprobedInventoryIsNotACapacityRefusal(t *testing.T) {
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	// A node record with no probe stamp — the restart window.
	require.NoError(t, repos.NewNodeRepo(st).Upsert(ctx, &types.Node{ID: testNodeID, Address: "127.0.0.1"}))
	require.NoError(t, repos.NewNodeLedgerRepo(st).EnsureExists(ctx, testNodeID))

	testRunner := runner.NewTestRunner()
	mgr := manager.NewTestRunnerManager(nil)
	mgr.SetDockerRunner(testRunner)
	mgr.SetProcessRunner(testRunner)
	c := NewController(st, mgr, log.NewTestLogger(),
		WithNodeID(testNodeID), WithGPUAdmitter(gpu.NewAdmitter(st)))

	svc := gpuService(t, st, "vllm", &types.GPURequest{})
	_, err := c.CreateInstance(ctx, svc, "vllm-0", 0)
	require.Error(t, err)
	assert.NotEqual(t, types.GPUReasonNoCapacity, gpu.ReasonOf(err),
		"never a capacity refusal: that is a claim about capacity the node has not made")
}
