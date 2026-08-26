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

	rec.reclaimGPUReservations(ctx, nil, time.Now())
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

	rec.reclaimGPUReservations(ctx, nil, time.Now())
	assert.Len(t, rows(t, st), 1, "a create in flight must survive the tick it started on")
}

// A create that ran out of retries holds its card DELIBERATELY: the
// retry path does not re-reserve, so the row is what lets it succeed.
// Reclaiming it because the status reads terminal hands the card to
// another service while the retry still believes it owns one.
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

			rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())
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

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())
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

	rec.reclaimGPUReservations(ctx, nil, time.Now())
	require.Len(t, rows(t, st), 1)
	assert.Equal(t, types.GPUResHolderIdle, rows(t, st)[0].Holder)
}

// GPUAssignments outlives the reservation, so a record that has already
// released still names its old card. Re-reserving from one of those books
// a card to something that gave it up — and nothing releases it again,
// because the release already happened.
func TestAdopt_IgnoresRecordsThatAlreadyReleased(t *testing.T) {
	failedAt := time.Now()
	cases := []struct {
		name  string
		mutot func(*types.Instance)
	}{
		{"stopped by the operator", func(i *types.Instance) {
			i.Status = types.InstanceStatusStopped
		}},
		{"health-restart tombstone", func(i *types.Instance) {
			i.Status = types.InstanceStatusFailed
			i.FailedAt = &failedAt
		}},
		{"deleted", func(i *types.Instance) {
			i.Status = types.InstanceStatusDeleted
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, rec, st := sweepFixture(t)
			svc := &types.Service{
				Name: "vllm", Namespace: "default", Scale: 1,
				Resources: types.Resources{GPU: &types.GPURequest{}},
			}
			require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))

			inst := types.Instance{
				ID: "released-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
				NodeID: sweepNodeID, GPUAssignments: []string{"GPU-1"},
			}
			tc.mutot(&inst)
			require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

			rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())
			assert.Empty(t, rows(t, st), "the card was given up; nothing may book it back")
		})
	}
}

// The same rule read from the other direction: a row left behind by a
// release that failed is reclaimable as soon as its instance reaches a
// status that no longer holds — not only once the record is Deleted.
func TestReclaim_FreesARowLeftBehindByAFailedRelease(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	inst := types.Instance{
		ID: "stopped-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusStopped,
	}
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: inst.ID, WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())
	assert.Empty(t, rows(t, st))
}

// A refusal that stands must not rewrite the record every minute: every
// write appends an unpruned version-history row.
func TestAdopt_RepeatedRefusalWritesTheInstanceOnce(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	svc := &types.Service{
		Name: "vllm", Namespace: "default", Scale: 1,
		Resources: types.Resources{GPU: &types.GPURequest{}},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "other",
		InstanceID: "other-1", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})
	other := types.Instance{
		ID: "other-1", Namespace: "default", ServiceName: "other",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
	}
	inst := types.Instance{
		ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
		GPUAssignments: []string{"GPU-1"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

	rec.reclaimGPUReservations(ctx, []types.Instance{other, inst}, time.Now())
	var first types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &first))

	for i := 0; i < 3; i++ {
		rec.reclaimGPUReservations(ctx, []types.Instance{other, inst}, time.Now())
	}
	var last types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &last))
	assert.Equal(t, first.UpdatedAt, last.UpdatedAt, "a standing refusal is not news")
}

// And it stops being true when the card frees up. A stale reason feeds
// the service-level summary and would outlive the problem it described.
func TestAdopt_ClearsTheFlagOnceTheAdoptionSucceeds(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	svc := &types.Service{
		Name: "vllm", Namespace: "default", Scale: 1,
		Resources: types.Resources{GPU: &types.GPURequest{}},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))
	inst := types.Instance{
		ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
		StatusReason: types.GPUReasonOverCommitted, StatusMessage: "no longer has room for it",
		GPUAssignments: []string{"GPU-1"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())

	require.Len(t, rows(t, st), 1)
	var stored types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &stored))
	assert.Empty(t, stored.StatusReason)
	assert.Empty(t, stored.StatusMessage)
}

// The grace window dates from when the instance list was read. The passes
// between can outlast it, and a row created after the snapshot would then
// look old enough to reclaim and be absent from a list that predates it.
func TestReclaim_GraceIsMeasuredFromTheSnapshotNotFromNow(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	// Written after the snapshot, but long enough ago that a grace window
	// dated from NOW would call it stale. That is the only shape the two
	// readings disagree about.
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: "created-after-the-list", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(2 * gpuReclaimGrace),
	})

	// The list was read three ticks ago; the sweep is only running now.
	rec.reclaimGPUReservations(ctx, nil, time.Now().Add(-3*gpuReclaimGrace))
	assert.Len(t, rows(t, st), 1,
		"a row written after the snapshot is not evidence about what the snapshot contained")
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

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())

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

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())
	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())

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

	rec.reclaimGPUReservations(ctx, []types.Instance{other, inst}, time.Now())

	assert.Len(t, rows(t, st), 1, "the overcommit must not be written into the ledger")

	var stored types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &stored))
	assert.Equal(t, types.InstanceStatusRunning, stored.Status,
		"a serving instance is never killed to tidy the books")
	assert.Equal(t, types.GPUReasonOverCommitted, stored.StatusReason)
}

// With no admitter wired the sweep does nothing at all — not "nothing it
// can see", nothing.
func TestReclaim_NoAdmitterDoesNothing(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	rec.SetGPUAdmitter(nil)
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "vllm",
		InstanceID: "ghost", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})

	rec.reclaimGPUReservations(ctx, nil, time.Now())
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

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())

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

	rec.reclaimGPUReservations(ctx, nil, time.Now())

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

// Every declared instance status, pinned by name.
//
// statusStillHoldsGPU falls through to "holds" for anything it does not
// name, which is the safe direction and a silent one: nothing in the
// build notices a new status landing there. The tree already has the
// shape of that bug — the design's release rule names Succeeded, which no
// instance can currently be, and the day one can it would arrive here
// holding its card forever with a green suite.
//
// So: adding an InstanceStatus without deciding this fails the build.
func TestStatusStillHoldsGPU_CoversEveryDeclaredStatus(t *testing.T) {
	failedAt := time.Now()
	expected := map[types.InstanceStatus]bool{
		// Released on the way in — the transition calls releaseGPU.
		types.InstanceStatusStopped: false,
		types.InstanceStatusDeleted: false,

		// Still holds: a container is running, or a retry needs the row.
		types.InstanceStatusPending:     true,
		types.InstanceStatusRunning:     true,
		types.InstanceStatusStalled:     true,
		types.InstanceStatusCreated:     true,
		types.InstanceStatusStarting:    true,
		types.InstanceStatusExited:      true,
		types.InstanceStatusUnknown:     true,
		types.InstanceStatusTerminating: true,

		// Failed splits on FailedAt, so it is asserted below instead.
		types.InstanceStatusFailed: true,
	}

	declared := []types.InstanceStatus{
		types.InstanceStatusPending, types.InstanceStatusRunning,
		types.InstanceStatusStopped, types.InstanceStatusFailed,
		types.InstanceStatusDeleted, types.InstanceStatusTerminating,
		types.InstanceStatusStalled, types.InstanceStatusCreated,
		types.InstanceStatusStarting, types.InstanceStatusExited,
		types.InstanceStatusUnknown,
	}
	require.Len(t, expected, len(declared),
		"a status was added or removed; decide whether it still holds its card")

	// Adopt asks a different question and must never be looser: it writes
	// a claim, where reclaim only declines to free one.
	adoptable := map[types.InstanceStatus]bool{
		types.InstanceStatusPending:     true,
		types.InstanceStatusRunning:     true,
		types.InstanceStatusStalled:     true,
		types.InstanceStatusCreated:     true,
		types.InstanceStatusStarting:    true,
		types.InstanceStatusTerminating: true,
		types.InstanceStatusFailed:      true, // FailedAt nil; the tombstone is asserted below
		types.InstanceStatusExited:      false,
		types.InstanceStatusUnknown:     false,
		types.InstanceStatusStopped:     false,
		types.InstanceStatusDeleted:     false,
	}
	require.Len(t, adoptable, len(declared),
		"a status was added or removed; decide whether its assignment may be adopted")

	for _, status := range declared {
		want, named := expected[status]
		require.True(t, named, "undecided status %q", status)
		assert.Equal(t, want, statusStillHoldsGPU(&types.Instance{Status: status}),
			"status %q", status)

		wantAdopt, named := adoptable[status]
		require.True(t, named, "undecided adopt for status %q", status)
		assert.Equal(t, wantAdopt, canAdoptDevices(&types.Instance{Status: status}),
			"adopt for status %q", status)
		if !want {
			assert.False(t, wantAdopt,
				"a status that has released cannot be adopted for: %q", status)
		}
	}

	// Terminating in particular: the container is live for the whole
	// drain, and the release comes after the Stopped or Deleted write.
	assert.True(t, statusStillHoldsGPU(&types.Instance{Status: types.InstanceStatusTerminating}))

	// The split inside Failed — a tombstone has released, a create still
	// retrying has not.
	assert.False(t, statusStillHoldsGPU(&types.Instance{
		Status: types.InstanceStatusFailed, FailedAt: &failedAt}))
	assert.True(t, statusStillHoldsGPU(&types.Instance{
		Status: types.InstanceStatusFailed}))
}

// Exited and Unknown read as dead everywhere else in the tree, and no
// transition out of them releases. Adopting one books a card to a dead
// container with nothing that will ever free it — the failure the shared
// predicate's "safe" default causes when the direction is reversed.
func TestAdopt_RefusesStatusesNothingWillEverRelease(t *testing.T) {
	for _, status := range []types.InstanceStatus{
		types.InstanceStatusExited,
		types.InstanceStatusUnknown,
	} {
		t.Run(string(status), func(t *testing.T) {
			ctx, rec, st := sweepFixture(t)
			svc := &types.Service{
				Name: "vllm", Namespace: "default", Scale: 1,
				Resources: types.Resources{GPU: &types.GPURequest{}},
			}
			require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))
			inst := types.Instance{
				ID: "dead-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
				NodeID: sweepNodeID, Status: status, GPUAssignments: []string{"GPU-1"},
			}
			require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

			rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())
			assert.Empty(t, rows(t, st), "nothing releases from %s, so nothing may claim for it", status)
		})
	}
}

// The startup window, where the agent has not reported devices yet. It is
// retryable and not the instance's fault, so it must not leave a note on
// a Running instance — the tick that succeeds would not think to clear a
// reason it did not write.
func TestAdopt_DoesNotFlagTheUnprobedWindow(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	// A node record with no probe stamp: runed up, agent not there yet.
	require.NoError(t, repos.NewNodeRepo(st).Upsert(ctx, &types.Node{
		ID: sweepNodeID, Address: "127.0.0.1",
	}))
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

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())

	var stored types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &stored))
	assert.Empty(t, stored.StatusReason, "a retryable hold is not news about the instance")
}

// A driver that stopped answering is not a card that left. The probe
// stamps its timestamp even on failure and writes an empty device list,
// so without this the sweep blames the hardware for a software fault.
func TestAdopt_DoesNotBlameTheCardForAFailedProbe(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	probed := time.Now()
	require.NoError(t, repos.NewNodeRepo(st).Upsert(ctx, &types.Node{
		ID: sweepNodeID, Address: "127.0.0.1", DevicesProbedAt: &probed,
		Devices: nil, DeviceProbeError: "nvidia-smi: command not found",
	}))
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

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())

	var stored types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &stored))
	assert.Empty(t, stored.StatusReason,
		"an inventory the node could not read is not evidence that a card is gone")
}

// A standing refusal must not append a node event every tick. The node's
// event window is finite, and two affected instances alternate messages
// so nothing folds — the GPU warnings would evict everything else.
func TestAdopt_StandingRefusalEmitsOneEvent(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	eventLog := &recordingEventLog{}
	rec.SetEventLog(eventLog)
	svc := &types.Service{
		Name: "vllm", Namespace: "default", Scale: 1,
		Resources: types.Resources{GPU: &types.GPURequest{}},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))
	writeRow(t, st, types.GPURes{
		DeviceUUID: "GPU-1", Namespace: "default", ServiceName: "other",
		InstanceID: "other-1", WholeDevice: true,
		Holder: types.GPUResHolderInstance, CreatedAt: aged(time.Hour),
	})
	other := types.Instance{
		ID: "other-1", Namespace: "default", ServiceName: "other",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
	}
	inst := types.Instance{
		ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
		GPUAssignments: []string{"GPU-1"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

	for i := 0; i < 4; i++ {
		rec.reclaimGPUReservations(ctx, []types.Instance{other, inst}, time.Now())
	}

	eventLog.mu.Lock()
	defer eventLog.mu.Unlock()
	require.Len(t, eventLog.events, 1, "the refusal is news once, not every minute")
	assert.Equal(t, types.GPUReasonOverCommitted, eventLog.events[0].Reason)
	assert.Contains(t, eventLog.events[0].Message, "default/other",
		"the operator has no command that renders a ledger, so the message names the holder")
}

// The clear has to cover every reason the flag can carry, not the two the
// common path happens to write. A bad vram string refuses with
// NoGpuCapacity; fixing the spec must take the note away with it. The
// same clear also removes a stale InventoryUnknown left by an older build
// that flagged the startup window.
func TestAdopt_ClearsEveryReasonItCanWrite(t *testing.T) {
	for _, stale := range []string{
		types.GPUReasonNoCapacity,
		types.GPUReasonInventoryUnknown,
		types.GPUReasonOverCommitted,
		types.GPUReasonDeviceMissing,
	} {
		t.Run(stale, func(t *testing.T) {
			ctx, rec, st := sweepFixture(t)
			svc := &types.Service{
				Name: "vllm", Namespace: "default", Scale: 1,
				Resources: types.Resources{GPU: &types.GPURequest{}},
			}
			require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))
			inst := types.Instance{
				ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
				NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
				StatusReason: stale, StatusMessage: "left over",
				GPUAssignments: []string{"GPU-1"},
			}
			require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

			rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())

			require.Len(t, rows(t, st), 1, "the adoption itself must succeed")
			var stored types.Instance
			require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &stored))
			assert.Empty(t, stored.StatusReason, "%s survived a successful adoption", stale)
			assert.Empty(t, stored.StatusMessage)
		})
	}
}

// An unparseable vram string is a refusal the operator has to fix in the
// spec, not a retryable hold — so unlike the startup window it does get
// flagged.
func TestAdopt_FlagsAnUnparseableVRAMRequest(t *testing.T) {
	ctx, rec, st := sweepFixture(t)
	svc := &types.Service{
		Name: "vllm", Namespace: "default", Scale: 1,
		Resources: types.Resources{GPU: &types.GPURequest{VRAM: "twenty gigs"}},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc))
	inst := types.Instance{
		ID: "live-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		NodeID: sweepNodeID, Status: types.InstanceStatusRunning,
		GPUAssignments: []string{"GPU-1"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &inst))

	rec.reclaimGPUReservations(ctx, []types.Instance{inst}, time.Now())

	var stored types.Instance
	require.NoError(t, st.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &stored))
	assert.Equal(t, types.GPUReasonNoCapacity, stored.StatusReason)
	assert.Equal(t, types.InstanceStatusRunning, stored.Status)
}
