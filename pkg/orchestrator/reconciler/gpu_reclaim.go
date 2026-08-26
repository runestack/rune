// reconciling the device ledgers against the instances that hold them.

package reconciler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/gpu"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// Event reasons for the sweep. Both are node-scoped: the ledger belongs
// to the node, and an operator chasing a card looks there first.
const (
	eventGpuReservationReclaimed = "GpuReservationReclaimed"
	eventGpuReservationAdopted   = "GpuReservationAdopted"
)

// gpuReclaimGrace is how old a reservation must be before the sweep will
// consider it orphaned. One GC tick: the row is written immediately
// before the instance record, so anything younger may simply be a create
// in flight.
const gpuReclaimGrace = garbageCollectionInterval

// SetGPUAdmitter wires device admission. Nil leaves the ledger sweep off.
func (r *Reconciler) SetGPUAdmitter(a *gpu.Admitter) { r.gpu = a }

// reclaimGPUReservations reconciles every node's ledger against the
// instances that should be holding it, in the two directions that can
// drift apart.
//
// Runs on the GC tick and takes the instance list already loaded there —
// the sweep is a repair path, not a control loop, and nothing depends on
// it being prompt. Both directions are best-effort: a node whose ledger
// cannot be read is logged and skipped, because one unreadable node must
// not stop the others being repaired.
func (r *Reconciler) reclaimGPUReservations(ctx context.Context, instances []types.Instance, snapshotAt time.Time) {
	if r.gpu == nil {
		return
	}

	held := make(map[string]bool, len(instances))
	for i := range instances {
		if statusStillHoldsGPU(&instances[i]) {
			held[instances[i].ID] = true
		}
	}

	ledgers, err := r.ledgers.List(ctx)
	if err != nil {
		r.logger.Error("GPU reclaim: could not list node ledgers", log.Err(err))
		return
	}

	// Measured from when the instance list was read, not from now: the
	// passes between the two can take longer than the grace window, and a
	// row created after the snapshot would then look both old enough to
	// reclaim and absent from an instance list that predates it.
	before := snapshotAt.UTC().Add(-gpuReclaimGrace)
	for _, ledger := range ledgers {
		dropped, err := r.gpu.ReclaimOrphans(ctx, ledger.NodeID, held, before)
		if err != nil {
			r.logger.Error("GPU reclaim failed",
				log.Str("node", ledger.NodeID), log.Err(err))
			continue
		}
		for _, row := range dropped {
			msg := fmt.Sprintf("freed %s: %s/%s no longer holds it",
				row.DeviceUUID, row.Namespace, row.ServiceName)
			r.logger.Info("Reclaimed orphaned GPU reservation",
				log.Str("node", ledger.NodeID),
				log.Str("device", row.DeviceUUID),
				log.Str("service", row.Namespace+"/"+row.ServiceName),
				log.Str("instance", row.InstanceID))
			r.emitNode(ctx, ledger.NodeID, types.EventLevelInfo, eventGpuReservationReclaimed, msg)
		}
	}

	// The snapshot above is still good for the adopt pass: reclaim only
	// drops rows whose instance is absent from held, and adopt only reads
	// rows belonging to instances that are in it.
	r.adoptOrphanedAssignments(ctx, instances, ledgers)
}

// adoptOrphanedAssignments is the other direction: an instance whose
// record names devices that no reservation covers.
//
// Left alone, that instance is invisible to admission — the card reads
// free while an engine holds memory on it, and the next service to land
// there overcommits. Re-reserving is the repair; refusing to is a
// decision to keep a healthy instance running and say so loudly, because
// the alternative is killing something that is serving to make the
// bookkeeping agree with itself.
func (r *Reconciler) adoptOrphanedAssignments(ctx context.Context, instances []types.Instance, ledgers []*types.NodeDeviceLedger) {
	byNode := make(map[string]*types.NodeDeviceLedger, len(ledgers))
	for _, l := range ledgers {
		byNode[l.NodeID] = l
	}

	for i := range instances {
		inst := &instances[i]
		// GPUAssignments outlives the reservation — releaseGPU does not
		// clear it, so a stopped instance or a failed tombstone still
		// names the card it used to be on. Re-reserving from that record
		// would book a card to something that gave it up, permanently:
		// the release already happened and will not happen again.
		if len(inst.GPUAssignments) == 0 || inst.NodeID == "" || !canAdoptDevices(inst) {
			continue
		}
		missing := unreservedDevices(byNode[inst.NodeID], inst)
		if len(missing) == 0 {
			continue
		}
		r.adoptInstanceDevices(ctx, inst, missing)
	}
}

// statusStillHoldsGPU reports whether reaching this status has already
// released the instance's devices. It is a claim about TRANSITIONS, not
// about the ledger: it cannot know whether a best-effort release
// actually landed, only whether one was attempted. Both sweep directions
// read it, so they cannot drift into disagreeing about who owns a card.
//
// FailedAt is the discriminator inside Failed, and it is not incidental:
// the health-restart path sets it and releases, while a create that ran
// out of retries deliberately leaves it unset because its retry does not
// re-reserve and needs the row kept. Stalled is that same case with the
// retries spent.
//
// A status not named here holds, which is the safe direction — a leak is
// repaired by the next transition; a card handed to two engines is not.
// Safe but silent, so every declared status is pinned by name in the
// tests rather than left to this default.
//
// Not the same question as sync.go's scale-slot filter, which keeps
// Stopped because a stopped instance still occupies its slot. It does not
// still hold its card.
func statusStillHoldsGPU(inst *types.Instance) bool {
	switch inst.Status {
	case types.InstanceStatusDeleted, types.InstanceStatusStopped:
		return false
	case types.InstanceStatusFailed:
		return inst.FailedAt == nil
	default:
		return true
	}
}

// canAdoptDevices reports whether an instance's assignment may be written
// back into the ledger.
//
// Deliberately NOT the same question as statusStillHoldsGPU, whose
// default is "still holds". That default is the safe one for reclaim,
// where being wrong leaks a card the next transition frees. Writing a
// claim is the opposite: Exited and Unknown read as dead everywhere else
// (classifyRecorded calls both a failed state) and no transition out of
// them releases, so a claim written for one is permanent and false.
func canAdoptDevices(inst *types.Instance) bool {
	if !statusStillHoldsGPU(inst) {
		return false
	}
	switch inst.Status {
	case types.InstanceStatusExited, types.InstanceStatusUnknown:
		return false
	}
	return true
}

// unreservedDevices are the instance's assigned devices that its own
// reservations do not cover. Matching is by instance ID as well as
// device: a row for the same card held by somebody else is not this
// instance's claim, and treating it as one would leave the overcommit in
// place while reporting it repaired.
func unreservedDevices(ledger *types.NodeDeviceLedger, inst *types.Instance) []string {
	covered := map[string]bool{}
	if ledger != nil {
		for _, row := range ledger.Reservations {
			if row.InstanceID == inst.ID {
				covered[row.DeviceUUID] = true
			}
		}
	}
	var missing []string
	for _, uuid := range inst.GPUAssignments {
		if !covered[uuid] {
			missing = append(missing, uuid)
		}
	}
	return missing
}

func (r *Reconciler) adoptInstanceDevices(ctx context.Context, inst *types.Instance, devices []string) {
	var service types.Service
	if err := r.store.Get(ctx, types.ResourceTypeService, inst.Namespace, inst.ServiceName, &service); err != nil {
		// No service means this instance should not be running either;
		// the orphan reap owns that, and guessing its request shape here
		// would reserve the wrong amount. Loud rather than silent: until
		// the reap runs, those devices read free while something holds
		// memory on them.
		r.logger.Warn("GPU: instance holds devices but its service is gone",
			log.Str("instance", inst.ID),
			log.Any("devices", devices), log.Err(err))
		return
	}
	if service.Resources.GPU == nil {
		// The spec dropped its gpu block while the instance kept running,
		// so there is no request to re-reserve against. Said out loud
		// because the resulting state is an invisible overcommit: those
		// devices read free until the next rollout replaces the instance.
		r.logger.Warn("GPU: instance holds devices its service no longer requests",
			log.Str("instance", inst.ID),
			log.Str("service", inst.Namespace+"/"+inst.ServiceName),
			log.Any("devices", devices))
		return
	}

	node, err := r.nodes.Get(ctx, inst.NodeID)
	if err != nil {
		r.logger.Debug("GPU adopt: could not read node inventory",
			log.Str("node", inst.NodeID), log.Err(err))
		return
	}
	if node.DeviceProbeError != "" {
		// The probe stamps DevicesProbedAt even when it fails, and writes
		// an empty device list — so admission would refuse every device as
		// missing and blame the hardware for a driver fault. An inventory
		// this node could not read is not evidence that a card is gone.
		r.logger.Debug("GPU adopt: skipping a node whose device probe failed",
			log.Str("node", inst.NodeID), log.Str("error", node.DeviceProbeError))
		return
	}

	adopted, err := r.gpu.AdoptAssignment(ctx, node, gpu.Request{
		NodeID:      inst.NodeID,
		Namespace:   inst.Namespace,
		ServiceName: inst.ServiceName,
		InstanceID:  inst.ID,
		GPU:         *service.Resources.GPU,
	}, devices)
	if err == nil {
		r.clearGPUFlag(ctx, inst)
		if len(adopted) == 0 {
			// The snapshot was stale and the rows already existed. Saying
			// so would report a repair that did not happen.
			return
		}
		msg := fmt.Sprintf("re-reserved %s for %s/%s: it was running on them with nothing recorded",
			strings.Join(adopted, ", "), inst.Namespace, inst.ServiceName)
		r.logger.Info("Adopted GPU assignment with no reservation",
			log.Str("instance", inst.ID),
			log.Any("devices", adopted))
		r.emitNode(ctx, inst.NodeID, types.EventLevelInfo, eventGpuReservationAdopted, msg)
		return
	}

	reason := gpu.ReasonOf(err)
	if reason == "" || reason == types.GPUReasonInventoryUnknown {
		// Either a store failure or the startup window before the agent
		// has reported devices. Neither is the instance's problem, and
		// flagging a retryable state leaves a note on a Running instance
		// that the tick which succeeds will not think to clear.
		r.logger.Debug("GPU adopt deferred",
			log.Str("instance", inst.ID), log.Err(err))
		return
	}
	// Event only when the note actually changed. A refusal that stands
	// repeats every tick, and a node's event window is fifty entries.
	if r.flagInstance(ctx, inst, reason, err.Error()) {
		r.emitNode(ctx, inst.NodeID, types.EventLevelWarn, reason, err.Error())
	}
}

// flagInstance records why an instance's devices could not be re-reserved
// WITHOUT touching its status. It stays exactly as running as it was —
// this is a note for the operator, not a lifecycle transition.
//
// Writes only on a change. A refusal that stands repeats every tick, and
// every write appends an unpruned version-history row.
func (r *Reconciler) flagInstance(ctx context.Context, inst *types.Instance, reason, message string) bool {
	wrote := false
	var fresh types.Instance
	err := r.store.UpdateFunc(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &fresh, func() error {
		wrote = false
		if fresh.StatusReason == reason && fresh.StatusMessage == message {
			return store.ErrSkipUpdate
		}
		fresh.StatusReason = reason
		fresh.StatusMessage = message
		fresh.UpdatedAt = time.Now()
		wrote = true
		return nil
	})
	if err != nil && !store.IsNotFoundError(err) {
		r.logger.Warn("Could not flag instance",
			log.Str("instance", inst.ID), log.Err(err))
		return false
	}
	return wrote && err == nil
}

// clearGPUFlag removes a refusal note once the adoption it described has
// succeeded. Without this the instance keeps reporting a reason that is
// no longer true, and StatusReason feeds the service-level summary.
//
// Only clears reasons this sweep writes: another controller's note about
// the same instance is not ours to drop.
func (r *Reconciler) clearGPUFlag(ctx context.Context, inst *types.Instance) {
	var fresh types.Instance
	err := r.store.UpdateFunc(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &fresh, func() error {
		switch fresh.StatusReason {
		case types.GPUReasonOverCommitted, types.GPUReasonDeviceMissing,
			types.GPUReasonNoCapacity, types.GPUReasonInventoryUnknown:
			fresh.StatusReason = ""
			fresh.StatusMessage = ""
			fresh.UpdatedAt = time.Now()
			return nil
		}
		return store.ErrSkipUpdate
	})
	if err != nil && !store.IsNotFoundError(err) {
		r.logger.Warn("Could not clear GPU flag",
			log.Str("instance", inst.ID), log.Err(err))
	}
}

// emitNode records a node-scoped event. Best-effort and nil-safe.
func (r *Reconciler) emitNode(ctx context.Context, nodeID string, level types.EventLevel, reason, message string) {
	if r.events == nil {
		return
	}
	if err := r.events.Emit(ctx, types.Event{
		Namespace: "", // the node record is cluster-scoped, and so is its ledger
		Kind:      "Node",
		Name:      nodeID,
		UID:       nodeID,
		Level:     level,
		Reason:    reason,
		Message:   message,
	}); err != nil {
		r.logger.Warn("Failed to emit node event",
			log.Str("node", nodeID), log.Err(err))
	}
}
