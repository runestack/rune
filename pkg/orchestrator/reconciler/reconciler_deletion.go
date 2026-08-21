package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// reconcileDeletion drives Kubernetes-style foreground deletion for a
// tombstoned service (RFC #129 Phase 4). It runs on the single-writer reconcile
// workqueue, so it never races another reconcile of the same service.
//
// The service's Finalizers list is the ordered work remaining
// (instance-cleanup, then volume-cleanup iff the service had claimTemplate
// volumes). Each finalizer is popped only after its work is fully done; when
// the list is empty the record is removed — the terminal transition. Because
// the tombstone + finalizer list are PERSISTED, an interrupted teardown resumes
// on the next reconcile (boot enqueue-all, the 30s resync, or the instance
// watch firing as instances vanish) with no separate recovery path. Removing
// the record only when finalizers are empty is what makes it provably outlive
// its instances and volumes, so orphans are impossible by construction.
func (r *Reconciler) reconcileDeletion(ctx context.Context, service *types.Service) error {
	ns, name := service.Namespace, service.Name

	var fins []types.FinalizerType
	if service.Metadata != nil {
		fins = service.Metadata.Finalizers
	}

	// Terminal: all finalizers cleared → remove the record. This is the SOLE
	// service-record remover in the system.
	if len(fins) == 0 {
		r.markDeletionOpCompleted(ctx, service)
		if err := r.store.Delete(ctx, types.ResourceTypeService, ns, name); err != nil {
			return fmt.Errorf("failed to remove tombstoned service record %s/%s: %w", ns, name, err)
		}
		r.logger.Info("Service teardown complete; record removed",
			log.Str("service", name), log.Str("namespace", ns))
		return nil
	}

	// Process the head of the ordered finalizer list.
	switch fins[0] {
	case types.FinalizerTypeInstanceCleanup:
		r.setDeletionOpStatus(ctx, service, types.DeletionOperationStatusDeletingInstances)
		done, err := r.cleanupServiceInstances(ctx, service)
		if err != nil {
			return err // requeue with backoff
		}
		if !done {
			// Instances still terminating. Self-enqueue so teardown progresses
			// promptly without depending on an instance-delete watch event
			// (which we may filter or miss); the queue coalesces duplicates.
			r.Enqueue(ns, name)
			return nil
		}
	case types.FinalizerTypeVolumeCleanup:
		r.setDeletionOpStatus(ctx, service, types.DeletionOperationStatusRunningFinalizers)
		if err := r.cleanupServiceVolumes(ctx, service); err != nil {
			return err // requeue; volumes must be reclaimed before the record is removed (#124)
		}
	default:
		// Unknown finalizer: drop it so a bad value can't wedge teardown forever.
		r.logger.Warn("Dropping unknown service finalizer",
			log.Str("service", name), log.Str("namespace", ns),
			log.Str("finalizer", string(fins[0])))
	}

	// Pop the completed finalizer atomically, guarded so a concurrent/retried
	// run never pops twice or skips one.
	head := fins[0]
	var fresh types.Service
	if err := r.store.UpdateFunc(ctx, types.ResourceTypeService, ns, name, &fresh, func() error {
		if fresh.Metadata == nil || len(fresh.Metadata.Finalizers) == 0 {
			return store.ErrSkipUpdate
		}
		if fresh.Metadata.Finalizers[0] != head {
			return store.ErrSkipUpdate // someone else already advanced it
		}
		fresh.Metadata.Finalizers = fresh.Metadata.Finalizers[1:]
		return nil
	}, store.WithOrchestrator()); err != nil {
		return fmt.Errorf("failed to advance finalizers for %s/%s: %w", ns, name, err)
	}

	// Drive the next finalizer (or the terminal record delete) without waiting
	// for the resync tick.
	r.Enqueue(ns, name)
	return nil
}

// cleanupServiceInstances stops and removes every instance of the service.
// Ported from InstanceCleanupFinalizer.Execute, driven by reconcile. It reports
// done=true only when zero live instances remain, so the finalizer is popped
// only once the service is genuinely instance-free. Every step is best-effort
// and idempotent: a crash between StopInstance and store.Delete is repaired on
// the next pass.
func (r *Reconciler) cleanupServiceInstances(ctx context.Context, service *types.Service) (bool, error) {
	var instances []types.Instance
	if err := r.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances); err != nil {
		return false, fmt.Errorf("failed to list instances for %s/%s: %w", service.Namespace, service.Name, err)
	}

	mine := make([]*types.Instance, 0, len(instances))
	for i := range instances {
		if instances[i].ServiceName == service.Name {
			mine = append(mine, &instances[i])
		}
	}
	if len(mine) == 0 {
		return true, nil // instance-cleanup complete
	}

	r.logger.Info("Tearing down service instances",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Int("count", len(mine)))

	// Withdraw every instance from the dataplane in one publish and take a
	// single shared drain window (RUNE-042 §4) — the per-instance
	// StopInstance/DeleteInstance calls below then see Terminating and skip
	// their own drains, so deleting a scale-N service costs one window, not N.
	r.instanceController.WithdrawServiceInstances(ctx, service, mine)

	removed := 0
	for _, inst := range mine {
		if err := r.instanceController.StopInstance(ctx, inst); err != nil {
			r.logger.Warn("Failed to stop instance during teardown",
				log.Str("instance", inst.ID), log.Err(err))
		}
		if err := r.instanceController.DeleteInstance(ctx, inst); err != nil {
			r.logger.Warn("Failed to delete instance during teardown",
				log.Str("instance", inst.ID), log.Err(err))
		}
		r.healthController.RemoveInstance(inst.ID)
		// Hard-remove the record. A failed container stop leaves a runner
		// orphan that cleanUpOrphanedInstances reaps; it must not block the
		// service record from converging to instance-free.
		if err := r.store.Delete(ctx, types.ResourceTypeInstance, service.Namespace, inst.ID); err != nil {
			r.logger.Warn("Failed to remove instance record during teardown",
				log.Str("instance", inst.ID), log.Err(err))
			continue
		}
		removed++
	}

	// store.Delete is synchronous, so re-list to confirm convergence within
	// THIS pass — done as soon as no records remain, without waiting for a
	// second reconcile. (If a delete failed above, `removed < len(mine)` and
	// the re-list still reflects reality.)
	if removed == len(mine) {
		return true, nil
	}
	var remaining []types.Instance
	if err := r.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &remaining); err != nil {
		return false, nil // couldn't confirm; caller self-enqueues to retry
	}
	for i := range remaining {
		if remaining[i].ServiceName == service.Name {
			return false, nil // some records still linger; retry next pass
		}
	}
	return true, nil
}

// cleanupServiceVolumes unbinds and deletes the service's owned claimTemplate
// volumes. Ported from VolumeCleanupFinalizer.Execute, with one deliberate
// hardening: it returns an error if any OWNED volume row could not be deleted,
// so the finalizer is NOT popped and the service record is NOT removed until the
// volume is actually gone. The old finalizer was best-effort because the
// store-orphan sweep + reclaimOrphanInstanceVolumes caught leaks; those safety
// nets are being retired, so volume reclaim must be authoritative here (the
// #124 leak class).
func (r *Reconciler) cleanupServiceVolumes(ctx context.Context, service *types.Service) error {
	owner := fmt.Sprintf("%s/%s", service.Namespace, service.Name)

	var vols []types.Volume
	if err := r.store.List(ctx, types.ResourceTypeVolume, service.Namespace, &vols); err != nil {
		return fmt.Errorf("failed to list volumes in %q: %w", service.Namespace, err)
	}

	// Instance names whose BoundClaim we must release (instance-cleanup already
	// removed the instance records, but a volume row may still point at one).
	instanceNames := make(map[string]struct{})
	var instances []types.Instance
	if err := r.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances); err == nil {
		for i := range instances {
			if instances[i].ServiceName == service.Name {
				instanceNames[service.Namespace+"/"+instances[i].Name] = struct{}{}
			}
		}
	}

	var deleteErrs []error
	unbound, deleted := 0, 0
	for i := range vols {
		v := &vols[i]

		// Step 1: release bind state on any volume claimed by one of our
		// (now-removed) instances or owned by this service — including
		// operator-managed `claim:` volumes, so no stale BoundClaim lingers.
		if _, claimed := instanceNames[v.BoundClaim]; claimed || v.OwnerService == owner {
			if v.BoundNode != "" || v.BoundClaim != "" {
				v.BoundNode = ""
				v.BoundClaim = ""
				if v.Handle != "" {
					v.Status = types.VolumeStatusAvailable
				}
				if err := r.store.Update(ctx, types.ResourceTypeVolume, v.Namespace, v.Name, v); err != nil {
					r.logger.Warn("Failed to release volume bind state during teardown",
						log.Str("volume", v.Name), log.Err(err))
				} else {
					unbound++
				}
			}
		}

		// Step 2: delete owned (claimTemplate) volume rows. Operator-managed
		// `claim:` volumes (no OwnerService match) are intentionally retained.
		if v.OwnerService != owner {
			continue
		}
		if err := r.store.Delete(ctx, types.ResourceTypeVolume, v.Namespace, v.Name); err != nil {
			deleteErrs = append(deleteErrs, fmt.Errorf("delete owned volume %s: %w", v.Name, err))
			continue
		}
		deleted++
		// The VolumeController's delete-watch runs the ReclaimPolicy
		// (Retain/Delete→driver) asynchronously.
	}

	r.logger.Info("Service volume cleanup pass",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Int("unbound", unbound), log.Int("deleted", deleted))

	if len(deleteErrs) > 0 {
		// Do NOT pop the finalizer: retry until every owned volume is gone.
		return fmt.Errorf("owned volume cleanup incomplete for %s: %w", owner, errors.Join(deleteErrs...))
	}
	return nil
}

// --- GetDeletionStatus compatibility shim ---------------------------------
//
// The CLI/dashboard poll a DeletionOperation record via GetDeletionStatus.
// reconcileDeletion advances that record as teardown progresses. It is a shim,
// not the source of truth (the tombstoned service is), so all writes are
// best-effort — a failed shim write never blocks the teardown.

// findDeletionOp returns the non-terminal deletion-operation shim for a service.
func (r *Reconciler) findDeletionOp(ctx context.Context, service *types.Service) (*types.DeletionOperation, bool) {
	var ops []types.DeletionOperation
	if err := r.store.List(ctx, types.ResourceTypeDeletionOperation, service.Namespace, &ops); err != nil {
		return nil, false
	}
	for i := range ops {
		if ops[i].ServiceName == service.Name && ops[i].Status != types.DeletionOperationStatusCompleted {
			return &ops[i], true
		}
	}
	return nil, false
}

func (r *Reconciler) setDeletionOpStatus(ctx context.Context, service *types.Service, status types.DeletionOperationStatus) {
	op, ok := r.findDeletionOp(ctx, service)
	if !ok || op.Status == status {
		return
	}
	op.Status = status
	if err := r.store.Update(ctx, types.ResourceTypeDeletionOperation, op.Namespace, op.ID, op); err != nil {
		r.logger.Debug("Failed to update deletion-op shim status", log.Err(err))
	}
}

func (r *Reconciler) markDeletionOpCompleted(ctx context.Context, service *types.Service) {
	op, ok := r.findDeletionOp(ctx, service)
	if !ok {
		return
	}
	op.Status = types.DeletionOperationStatusCompleted
	op.DeletedInstances = op.TotalInstances
	now := time.Now()
	op.EndTime = &now
	if err := r.store.Update(ctx, types.ResourceTypeDeletionOperation, op.Namespace, op.ID, op); err != nil {
		r.logger.Debug("Failed to mark deletion-op shim completed", log.Err(err))
	}
}
