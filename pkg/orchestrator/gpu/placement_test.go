package gpu_test

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/orchestrator/gpu"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func devP(uuid, product string, vram int64) types.GPUDevice {
	return types.GPUDevice{UUID: uuid, Vendor: "nvidia", Product: product, VRAMBytes: vram}
}

func res(uuid, ns, svc string, vram int64, whole bool) types.GPURes {
	return types.GPURes{
		DeviceUUID: uuid, Namespace: ns, ServiceName: svc,
		VRAMBytes: vram, WholeDevice: whole,
		Holder: types.GPUResHolderInstance, CreatedAt: time.Now(),
	}
}

// Best-fit: pack the fullest device that still fits, so roomier cards
// stay available for larger requests.
func TestChooseDevices_BestFitPacksTheFullestThatFits(t *testing.T) {
	devices := []types.GPUDevice{
		devP("GPU-empty", "L40S", gi48),
		devP("GPU-half", "L40S", gi48),
	}
	ledger := &types.NodeDeviceLedger{Reservations: []types.GPURes{
		res("GPU-half", "prod", "tei", 20<<30, false),
	}}

	p, err := gpu.ChooseDevices(devices, ledger, "prod", types.GPURequest{VRAM: "10Gi"})
	require.NoError(t, err)
	assert.Equal(t, []string{"GPU-half"}, p.DeviceUUIDs,
		"the fuller card is chosen so the empty one stays whole")
}

// The tie-break that outranks best-fit. Plain best-fit prefers the
// fullest card that fits — which is by construction the one with the most
// neighbours — so on hardware with no inter-tenant memory isolation the
// default policy would actively maximise cross-tenant sharing.
func TestChooseDevices_PrefersOwnNamespaceOverAFullerForeignCard(t *testing.T) {
	devices := []types.GPUDevice{
		devP("GPU-foreign", "L40S", gi48), // fuller: plain best-fit would pick this
		devP("GPU-own", "L40S", gi48),
	}
	ledger := &types.NodeDeviceLedger{Reservations: []types.GPURes{
		res("GPU-foreign", "staging", "other", 30<<30, false),
		res("GPU-own", "prod", "mine", 10<<30, false),
	}}

	p, err := gpu.ChooseDevices(devices, ledger, "prod", types.GPURequest{VRAM: "5Gi"})
	require.NoError(t, err)
	assert.Equal(t, []string{"GPU-own"}, p.DeviceUUIDs)
	assert.False(t, p.CrossNamespace)
}

func TestChooseDevices_PrefersEmptyOverForeign(t *testing.T) {
	devices := []types.GPUDevice{
		devP("GPU-foreign", "L40S", gi48),
		devP("GPU-empty", "L40S", gi48),
	}
	ledger := &types.NodeDeviceLedger{Reservations: []types.GPURes{
		res("GPU-foreign", "staging", "other", 30<<30, false),
	}}

	p, err := gpu.ChooseDevices(devices, ledger, "prod", types.GPURequest{VRAM: "5Gi"})
	require.NoError(t, err)
	assert.Equal(t, []string{"GPU-empty"}, p.DeviceUUIDs)
}

// Landing on another namespace's card is allowed, but never silently: a
// shared device is a shared trust domain and the operator should learn it
// from an event rather than from an incident.
func TestChooseDevices_ForeignCardIsFlagged(t *testing.T) {
	devices := []types.GPUDevice{devP("GPU-1", "L40S", gi48)}
	ledger := &types.NodeDeviceLedger{Reservations: []types.GPURes{
		res("GPU-1", "staging", "other", 10<<30, false),
	}}

	p, err := gpu.ChooseDevices(devices, ledger, "prod", types.GPURequest{VRAM: "5Gi"})
	require.NoError(t, err)
	assert.True(t, p.CrossNamespace)
	assert.Equal(t, []string{"staging"}, p.OtherNamespaces)
}

// The documented cost of that tie-break, pinned so it is a known trade
// rather than a surprise: two half-full cards from two namespaces refuse
// a request plain best-fit would have placed.
func TestChooseDevices_TieBreakCostsPacking(t *testing.T) {
	devices := []types.GPUDevice{
		devP("GPU-1", "L40S", gi24),
		devP("GPU-2", "L40S", gi24),
	}
	ledger := &types.NodeDeviceLedger{Reservations: []types.GPURes{
		res("GPU-1", "a", "x", 12<<30, false),
		res("GPU-2", "b", "y", 12<<30, false),
	}}

	// It still places — into tier 3 — and says so.
	p, err := gpu.ChooseDevices(devices, ledger, "prod", types.GPURequest{VRAM: "5Gi"})
	require.NoError(t, err)
	assert.True(t, p.CrossNamespace, "the only option was another namespace's card")
	assert.Len(t, p.OtherNamespaces, 1)
}

func TestChooseDevices_WholeDeviceNeedsAnEmptyCard(t *testing.T) {
	devices := []types.GPUDevice{devP("GPU-1", "L40S", gi48), devP("GPU-2", "L40S", gi48)}
	ledger := &types.NodeDeviceLedger{Reservations: []types.GPURes{
		res("GPU-1", "prod", "tei", 1<<30, false),
	}}

	p, err := gpu.ChooseDevices(devices, ledger, "prod", types.GPURequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"GPU-2"}, p.DeviceUUIDs, "a whole-device request cannot join a shared card")
}

func TestChooseDevices_MultiDeviceRefusesMismatchedProducts(t *testing.T) {
	devices := []types.GPUDevice{devP("GPU-1", "L40S", gi48), devP("GPU-2", "A100", gi48)}

	_, err := gpu.ChooseDevices(devices, &types.NodeDeviceLedger{}, "prod", types.GPURequest{Count: 2})
	require.Error(t, err, "tensor parallelism across mismatched cards fails at load with an error naming neither")
	assert.Contains(t, err.Error(), "allowHeterogeneous")

	p, err := gpu.ChooseDevices(devices, &types.NodeDeviceLedger{}, "prod",
		types.GPURequest{Count: 2, AllowHeterogeneous: true})
	require.NoError(t, err, "opting in must work")
	assert.Len(t, p.DeviceUUIDs, 2)
}

func TestChooseDevices_MoreDevicesThanExist(t *testing.T) {
	devices := []types.GPUDevice{devP("GPU-1", "L40S", gi48)}
	_, err := gpu.ChooseDevices(devices, &types.NodeDeviceLedger{}, "prod", types.GPURequest{Count: 2})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "asks for 2 GPUs; this node has 1")
}

// Ghost devices get their own message: the driver is fine, the cards went
// away. Blaming the driver here sends the operator hunting in the wrong
// place.
func TestChooseDevices_AllMissingBlamesTheCardsNotTheDriver(t *testing.T) {
	devices := []types.GPUDevice{
		{UUID: "GPU-1", VRAMBytes: gi48, Missing: true},
		{UUID: "GPU-2", VRAMBytes: gi48, Missing: true},
	}
	_, err := gpu.ChooseDevices(devices, &types.NodeDeviceLedger{}, "prod", types.GPURequest{})
	require.Error(t, err)
	assert.Equal(t, types.GPUReasonDeviceMissing, gpu.ReasonOf(err))
	assert.Contains(t, err.Error(), "marked missing")
}

func TestChooseDevices_NoDevicesAtAll(t *testing.T) {
	_, err := gpu.ChooseDevices(nil, &types.NodeDeviceLedger{}, "prod", types.GPURequest{})
	require.Error(t, err)
	assert.Equal(t, types.GPUReasonNoCapacity, gpu.ReasonOf(err))
	assert.Contains(t, err.Error(), "no GPUs")
}

// The system reserve is withheld from every device: a flat floor plus a
// per-instance term for CUDA context overhead that is charged to nobody
// otherwise. Without it the first shared card overcommits by ~2GiB at
// five co-tenants and blames whichever engine allocated last.
func TestReservedBytes_FloorPlusPerInstance(t *testing.T) {
	ledger := &types.NodeDeviceLedger{Reservations: []types.GPURes{
		res("GPU-1", "prod", "a", 1<<30, false),
		res("GPU-1", "prod", "b", 1<<30, false),
	}}
	want := types.DefaultReservedVRAMFloor + 2*types.DefaultReservedVRAMPerInstance
	assert.Equal(t, want, ledger.ReservedBytes("GPU-1"))

	// An empty device still holds back the floor.
	assert.Equal(t, types.DefaultReservedVRAMFloor, ledger.ReservedBytes("GPU-other"))
}

// A request that exactly exhausts the card must be refused, because the
// reserve is not available to it.
func TestChooseDevices_ReserveIsNotAvailableCapacity(t *testing.T) {
	devices := []types.GPUDevice{devP("GPU-1", "L40S", gi24)}
	_, err := gpu.ChooseDevices(devices, &types.NodeDeviceLedger{}, "prod",
		types.GPURequest{VRAM: "24Gi"})
	require.Error(t, err, "the full device size must not be requestable — the system reserve is withheld")
}

// A whole-device holder does not consume VRAMBytes, so a ledger must not
// count it as zero-bytes-used and admit a co-tenant by arithmetic.
func TestRequestedBytes_IgnoresWholeDeviceHolders(t *testing.T) {
	ledger := &types.NodeDeviceLedger{Reservations: []types.GPURes{
		res("GPU-1", "prod", "vllm", 0, true),
	}}
	assert.EqualValues(t, 0, ledger.RequestedBytes("GPU-1"))
	assert.True(t, ledger.HasWholeDeviceHolder("GPU-1"),
		"the exclusion has to come from the flag, not from the byte count")
}
