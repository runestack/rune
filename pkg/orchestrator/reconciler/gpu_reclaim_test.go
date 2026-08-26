package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/gpu"
	"github.com/runestack/rune/pkg/orchestrator/health"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sweepNodeID = "node-sweep-1"

// sweepFixture is a reconciler with GPU admission wired, a node carrying
// one 48Gi card, and an empty ledger.
func sweepFixture(t *testing.T) (context.Context, *Reconciler, store.Store) {
	t.Helper()
	ctx := context.Background()
	st := setupStore(t)
	logger := log.NewTestLogger()
	instCtrl := instancectl.NewFakeController()
	rec := New(st, instCtrl, health.NewController(logger, st, manager.NewTestRunnerManager(nil), instCtrl), logger)
	rec.SetGPUAdmitter(gpu.NewAdmitter(st))

	probed := time.Now()
	require.NoError(t, repos.NewNodeRepo(st).Upsert(ctx, &types.Node{
		ID: sweepNodeID, Address: "127.0.0.1", DevicesProbedAt: &probed,
		Devices: []types.GPUDevice{{
			UUID: "GPU-1", Vendor: "nvidia", Product: "NVIDIA L40S", VRAMBytes: 48 << 30,
		}},
	}))
	require.NoError(t, repos.NewNodeLedgerRepo(st).EnsureExists(ctx, sweepNodeID))
	return ctx, rec, st
}

// writeRow puts a reservation straight into the ledger, bypassing
// admission — which is exactly the state the sweep exists to find.
func writeRow(t *testing.T, st store.Store, row types.GPURes) {
	t.Helper()
	repo := repos.NewNodeLedgerRepo(st)
	l, err := repo.Get(context.Background(), sweepNodeID)
	require.NoError(t, err)
	l.Reservations = append(l.Reservations, row)
	require.NoError(t, st.Update(context.Background(), types.ResourceTypeNodeLedger, "", sweepNodeID, l))
}

func rows(t *testing.T, st store.Store) []types.GPURes {
	t.Helper()
	l, err := repos.NewNodeLedgerRepo(st).Get(context.Background(), sweepNodeID)
	require.NoError(t, err)
	return l.Reservations
}

func aged(d time.Duration) time.Time { return time.Now().UTC().Add(-d) }

// The state the sweep is built for: a crash between the reservation and
// the instance record. Nothing else can ever free that row — there is no
// instance to stop, fail or delete.
func TestReclaim_FreesARowWhoseInstanceNeverExisted(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: "ghost", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})

	rec.reclaimGPUReservations(ctx, nil)
	assert.Empty(t, rows(t, st))
}

// The grace window. A reservation is written just before the instance
// record, so a sweep with no grace lands in that gap and reclaims a card
// from a create that is milliseconds from finishing.
func TestReclaim_LeavesAFreshRowAlone(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: "being-written", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: time.Now().UTC(),
	})

	rec.reclaimGPUReservations(ctx, nil)
	assert.Len(t, rows(t, st), 1, "a create in flight must survive the tick it started on")
}

// The trap in the design's own wording. §8.6 says to reclaim rows with
// "no non-terminal instance", and Failed is terminal — but a create that
// ran out of retries holds its card DELIBERATELY, because the retry path
// does not re-reserve. Reclaiming it hands the card to another service
// while the retry still believes it owns one.
func TestReclaim_LeavesRetryingAndStalledInstancesHoldingTheirCards(t *testing.T) {
	for _, status := range []types.InstanceStatus{
		types.InstanceStatusFailed,
		types.InstanceStatusStalled,
	} {
		t.Run(string(status), func(t *testing.T) {
			ctx, rec, st := sweepFixture(t)
			inst := types.Instance{
				ID: "inst-" + string(status), Name: "vllm-0", Namespace: "default",
				ServiceName: "vllm", NodeID: sweepNodeID, Status: status,
			}
			writeRow(t, st, types.GPURes{
				DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
				InstanceID: inst.ID, WholeDevice: true,
				Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
			})

			rec.reclaimGPUReservations(ctx, []types.Instance{inst})
			require.Len(t, rows(t, st), 1, "the retry has nothing to re-reserve with")
			assert.Equal(t, inst.ID, rows(t, st)[0].InstanceID)
		})
	}
}

// Deleted is the one status with nothing left to hold a card, so a row
// that outlived a release failure gets repaired here.
func TestReclaim_FreesARowWhoseInstanceIsDeleted(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	inst := types.Instance{
		ID: "gone", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusDeleted,
	}
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: inst.ID, WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})

	rec.reclaimGPUReservations(ctx, []types.Instance{inst})
	assert.Empty(t, rows(t, st))
}

// An idle hold has no instance by construction, so every condition the
// sweep tests is met — and it must still be left alone. Only the
// controller that parked the model knows whether it is still wanted.
func TestReclaim_NeverTouchesAnIdleHold(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: "", WholeDevice: true,
		Holder: types.GPUResHolderIdle, CreatedAt: aged(time.Hour),
	})

	rec.reclaimGPUReservations(ctx, nil)
	require.Len(t, rows(t, st), 1)
	assert.Equal(t, types.GPUResHolderIdle, rows(t, st)[0].Holder)
}

// The other direction: an instance running on a card the ledger does not
// know about. Left alone the card reads free while an engine holds memory
// on it, and the next service to land there overcommits.
func TestAdopt_ReReservesADeviceTheLedgerLost(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	svc := &types.Service{
		Name: "vllm", Namespace: "default", Scale: 1,
		Resources: types.Resources{GPU: &types.GPURequest{}},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))

	inst := types.Instance{
		ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
		GPUAssignments: []string{"GPU-1"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

	rec.reclaimGPUReservations(ctx, []types.Instance{inst})

	got := rows(t, st)
	require.Len(t, got, 1)
	assert.Equal(t, "GPU-1", got[0].DeviceUUID)
	assert.Equal(t, inst.ID, got[0].InstanceID)
	assert.True(t, got[0].WholeDevice)
}

// Running the sweep twice must not double-count the adoption. A second
// row for the same card would refuse the next service that should fit.
func TestAdopt_IsIdempotentAcrossTicks(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	svc := &types.Service{
		Name: "vllm", Namespace: "default", Scale: 1,
		Resources: types.Resources{GPU: &types.GPURequest{VRAM: "20Gi"}},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))
	inst := types.Instance{
		ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
		GPUAssignments: []string{"GPU-1"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

	rec.reclaimGPUReservations(ctx, []types.Instance{inst})
	rec.reclaimGPUReservations(ctx, []types.Instance{inst})

	got := rows(t, st)
	require.Len(t, got, 1, "the second tick must not add a second row for the same card")
	assert.EqualValues(t, 20<<30, got[0].VRAMBytes)
}

// Rune does not kill a healthy serving instance to make its own
// bookkeeping tidy. The instance keeps running and the operator gets told.
func TestAdopt_FlagsRatherThanKillsWhenTheCardIsFull(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	svc := &types.Service{
		Name: "vllm", Namespace: "default", Scale: 1,
		Resources: types.Resources{GPU: &types.GPURequest{}},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))

	// Somebody else already holds the card whole.
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "other",
		InstanceID: "other-1", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})
	other := types.Instance{
		ID: "other-1", Name: "other-0", Namespace: "default", ServiceName: "other",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
	}

	inst := types.Instance{
		ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
		GPUAssignments: []string{"GPU-1"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

	rec.reclaimGPUReservations(ctx, []types.Instance{other, inst})

	assert.Len(t, rows(t, st), 1, "the overcommit must not be written into the ledger")

	var stored types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &stored))
	assert.Equal(t, types.InstanceStatusRunning, stored.Status,
		"a serving instance is never killed to tidy the books")
	assert.Equal(t, types.GPUReasonOverCommitted, stored.StatusReason)
}

// With no admitter wired the GC tick must not read a ledger at all — the
// feature has to be absent on a box with no GPUs, not merely inert.
func TestReclaim_NoAdmitterDoesNothing(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	rec.SetGPUAdmitter(nil)
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: "ghost", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})

	rec.reclaimGPUReservations(ctx, nil)
	assert.Len(t, rows(t, st), 1)
}

// The sweep is reached from the GC tick, not only from its own test.
func TestGarbageCollection_RunsTheGPUSweep(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: "ghost", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})

	require.NoError(t, rec.runGarbageCollection(ctx))
	assert.Empty(t, rows(t, st))
}

// The ghost-device state: the instance is running on a card the node has
// stopped reporting. There is nothing to re-reserve against, and the
// instance still must not be killed for it.
func TestAdopt_FlagsAnInstanceRunningOnAVanishedDevice(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	svc := &types.Service{
		Name: "vllm", Namespace: "default", Scale: 1,
		Resources: types.Resources{GPU: &types.GPURequest{}},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))

	inst := types.Instance{
		ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
		GPUAssignments: []string{"GPU-GONE"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

	rec.reclaimGPUReservations(ctx, []types.Instance{inst})

	assert.Empty(t, rows(t, st), "capacity is never claimed against hardware the node does not report")
	var stored types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &stored))
	assert.Equal(t, types.InstanceStatusRunning, stored.Status)
	assert.Equal(t, types.GPUReasonDeviceMissing, stored.StatusReason)
}

// A reclaimed card is the operator's only notice that something held one
// it should not have. Node-scoped, because the ledger is.
func TestReclaim_EmitsANodeScopedEvent(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	eventLog := &recordingEventLog{}
	rec.SetEventLog(eventLog)
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: "ghost", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})

	rec.reclaimGPUReservations(ctx, nil)

	eventLog.mu.Lock()
	defer eventLog.mu.Unlock()
	require.Len(t, eventLog.events, 1)
	got := eventLog.events[0]
	assert.Equal(t, eventGpuReservationReclaimed, got.Reason)
	assert.Equal(t, "Node", got.Kind)
	assert.Equal(t, sweepNodeID, got.Name)
	assert.Empty(t, got.Namespace, "the ledger is cluster-scoped, like the node record")
	assert.Contains(t, got.Message, "GPU-1")
	assert.Contains(t, got.Message, "default/vllm")
}
