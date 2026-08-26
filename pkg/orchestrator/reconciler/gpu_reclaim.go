// reconciling the device ledgers against the instances that hold them.

package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/gpu"
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

// SetGPUAdmitter wires device admission. Nil leaves the ledger sweep off,
// which is what a control plane with no GPU nodes wants — it also means
// no ledger reads on the GC tick.
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
func (r *Reconciler) reclaimGPUReservations(ctx context.Context, instances []types.Instance) {
	if r.gpu == nil {
		return
	}

	held := make(map[string]bool, len(instances))
	for i := range instances {
		// Deleted is the one status with nothing left to hold a card:
		// every other state either still has a container or is a
		// tombstone whose retry needs its reservation kept.
		if instances[i].Status != types.InstanceStatusDeleted {
			held[instances[i].ID] = true
		}
	}

	ledgers, err := r.ledgers.List(ctx)
	if err != nil {
		r.logger.Error("GPU reclaim: could not list node ledgers", log.Err(err))
		return
	}

	before := time.Now().UTC().Add(-gpuReclaimGrace)
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
		if len(inst.GPUAssignments) == 0 ||
			inst.Status == types.InstanceStatusDeleted ||
			inst.NodeID == "" {
			continue
		}
		missing := unreservedDevices(byNode[inst.NodeID], inst)
		if len(missing) == 0 {
			continue
		}
		r.adoptInstanceDevices(ctx, inst, missing)
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
func (r *Reconciler) flagInstance(ctx context.Context, inst *types.Instance, reason, message string) {
	var fresh types.Instance
	err := r.store.UpdateFunc(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &fresh, func() error {
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
