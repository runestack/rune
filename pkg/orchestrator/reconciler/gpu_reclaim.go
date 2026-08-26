// reconciling the device ledgers against the instances that hold them.

package reconciler

import (
	"context"
	"fmt"
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
		if holdsAReservation(&instances[i]) {
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
			msg := fmt.Sprintf("reclaimed %s from %s/%s: the instance holding it no longer exists",
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
		if len(inst.GPUAssignments) == 0 || inst.NodeID == "" || !holdsAReservation(inst) {
			continue
		}
		missing := unreservedDevices(byNode[inst.NodeID], inst)
		if len(missing) == 0 {
			continue
		}
		r.adoptInstanceDevices(ctx, inst, missing)
	}
}

// holdsAReservation reports whether an instance should still own rows in
// the ledger. It mirrors the release rule exactly, and both sweep
// directions read it, so they cannot drift into disagreeing about who
// owns a card.
//
// FailedAt is the discriminator inside Failed, and it is not incidental:
// the health-restart path sets it and releases, while a create that ran
// out of retries deliberately leaves it unset because its retry does not
// re-reserve and needs the row kept. Stalled is that same case with the
// retries spent.
func holdsAReservation(inst *types.Instance) bool {
	switch inst.Status {
	case types.InstanceStatusDeleted, types.InstanceStatusStopped:
		return false
	case types.InstanceStatusFailed:
		return inst.FailedAt == nil
	default:
		return true
	}
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
		// would reserve the wrong amount.
		r.logger.Debug("GPU adopt: skipping instance whose service is gone",
			log.Str("instance", inst.ID), log.Err(err))
		return
	}
	if service.Resources.GPU == nil {
		// The assignment outlived the request that produced it — the spec
		// dropped its gpu block while the instance kept running. Nothing
		// to re-reserve; the next rollout replaces it.
		return
	}

	node, err := r.nodes.Get(ctx, inst.NodeID)
	if err != nil {
		r.logger.Warn("GPU adopt: could not read node inventory",
			log.Str("node", inst.NodeID), log.Err(err))
		return
	}

	err = r.gpu.AdoptAssignment(ctx, node, gpu.Request{
		NodeID:      inst.NodeID,
		Namespace:   inst.Namespace,
		ServiceName: inst.ServiceName,
		InstanceID:  inst.ID,
		GPU:         *service.Resources.GPU,
	}, devices)
	if err == nil {
		r.clearGPUFlag(ctx, inst)
		msg := fmt.Sprintf("re-reserved %v for %s: the instance was running on them with no reservation",
			devices, inst.Name)
		r.logger.Info("Adopted GPU assignment with no reservation",
			log.Str("instance", inst.ID),
			log.Any("devices", devices))
		r.emitNode(ctx, inst.NodeID, types.EventLevelInfo, eventGpuReservationAdopted, msg)
		return
	}

	reason := gpu.ReasonOf(err)
	if reason == "" {
		// A store failure rather than a refusal — nothing to flag on the
		// instance, and the next tick tries again.
		r.logger.Error("GPU adopt failed",
			log.Str("instance", inst.ID), log.Err(err))
		return
	}
	r.flagInstance(ctx, inst, reason, err.Error())
	r.emitNode(ctx, inst.NodeID, types.EventLevelWarn, reason, err.Error())
}

// flagInstance records why an instance's devices could not be re-reserved
// WITHOUT touching its status. It stays exactly as running as it was —
// this is a note for the operator, not a lifecycle transition.
//
// Writes only on a change. A refusal that stands repeats every tick, and
// every write appends an unpruned version-history row.
func (r *Reconciler) flagInstance(ctx context.Context, inst *types.Instance, reason, message string) {
	var fresh types.Instance
	err := r.store.UpdateFunc(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &fresh, func() error {
		if fresh.StatusReason == reason && fresh.StatusMessage == message {
			return store.ErrSkipUpdate
		}
		fresh.StatusReason = reason
		fresh.StatusMessage = message
		fresh.UpdatedAt = time.Now()
		return nil
	})
	if err != nil {
		r.logger.Warn("Could not flag instance",
			log.Str("instance", inst.ID), log.Err(err))
	}
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
		case types.GPUReasonOverCommitted, types.GPUReasonDeviceMissing:
			fresh.StatusReason = ""
			fresh.StatusMessage = ""
			fresh.UpdatedAt = time.Now()
			return nil
		}
		return store.ErrSkipUpdate
	})
	if err != nil {
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
