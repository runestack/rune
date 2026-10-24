// per-service reconcile: the sync entry point, instance
// collection, scale-down, and existing-instance handling.

package reconciler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// reconcileService ensures that a single service's instances match the desired state
func (r *Reconciler) reconcileService(ctx context.Context, service *types.Service) error {
	// reconcileServices iterates a service snapshot captured at the top of the
	// cycle. A service deleted mid-cycle (DeleteService / `rune release delete`)
	// is still in that stale list, so acting on the stale copy here re-creates
	// instances — and their claimTemplate volumes — for a service the deletion
	// task already removed. Those instances are reaped by the store-orphan
	// sweep, but the re-created volumes leak (orphaned, bound to a gone
	// instance). Re-read the service from the store so a just-deleted service
	// drops out of reconciliation; any error (not-found or transient) means
	// "don't create against stale state this cycle" and the next tick retries.
	var fresh types.Service
	if err := r.store.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &fresh); err != nil {
		r.logger.Debug("Skipping reconciliation: service not readable from store (deleted mid-cycle?)",
			log.Str("service", service.Name),
			log.Str("namespace", service.Namespace),
			log.Err(err))
		return nil
	}
	service = &fresh

	r.logger.Debug("Reconciling service",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace))

	// Foreground deletion takes precedence over spec reconciliation: a
	// tombstoned service (DeletionTimestamp set) is torn down by
	// reconcileDeletion — instances → volumes → record removal — and is never
	// reconciled toward its spec. This branch replaces the old
	// "skip if Status==Deleted" guard; keying off DeletionTimestamp is what
	// makes the teardown actually run (a bare Status==Deleted would just be
	// skipped) (RFC #129 Phase 4).
	if service.Metadata != nil && service.Metadata.DeletionTimestamp != nil {
		return r.reconcileDeletion(ctx, service)
	}

	// Scale down if needed.
	//
	// Stateless services no longer come through here: their excess is part of
	// the update plan, decided in one place alongside retirement and creation
	// (RUNE-042 §8.1). Two functions independently deciding "which instances
	// die" is exactly how the surged replacement gets torn down out from under
	// an in-flight update — this pass would see Scale+1 instances, call one of
	// them excess, and retire it with no regard for readiness.
	//
	// Stateful services still use it: their slot-based path does not run the
	// planner yet (Phase 5), so nothing else would remove instances above the
	// desired scale.
	if serviceHasStableIdentity(service) {
		if err := r.scaleDownService(ctx, service); err != nil {
			r.logger.Error("Error scaling down service",
				log.Str("service", service.Name),
				log.Err(err))
			// Continue with the rest of reconciliation
		}
	}

	// Ensure we have the right number of instances and they're up to date
	if err := r.ensureServiceInstances(ctx, service); err != nil {
		r.logger.Error("Error ensuring service instances",
			log.Str("service", service.Name),
			log.Err(err))
		// Continue with the rest of reconciliation
	}

	// Update service status based on the latest instance data
	if err := r.updateServiceStatus(ctx, service); err != nil {
		r.logger.Error("Failed to update service status",
			log.Str("service", service.Name),
			log.Err(err))
	}

	// Keep dataplane endpoint sets fresh (container IP backfill, VIP proxy,
	// ingress upstream via service VIP).
	if len(service.Ports) > 0 {
		r.instanceController.RepublishService(ctx, service)
	}

	return nil
}

// getServiceInstances retrieves and filters instances for a specific service
// Marks running instances that belong to this service as not orphaned
func (r *Reconciler) getServiceInstances(ctx context.Context, service *types.Service) (*ServiceInstanceData, error) {
	// Get running instances from all runners
	runningInstances, err := r.instanceController.CollectRunningInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to collect running instances: %w", err)
	}

	// Get existing instances for this service from store
	var storeInstances []types.Instance
	err = r.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &storeInstances)
	if err != nil {
		return nil, fmt.Errorf("failed to get instances for service: %w", err)
	}

	r.logger.Debug("Retrieved instances for service",
		log.Str("service", service.Name),
		log.Int("count", len(storeInstances)))

	// Filter instances for this service, skipping deleted ones
	var serviceInstances []types.Instance
	var orphanedInstances []*types.Instance

	// Track which running instances belong to this service
	serviceRunningInstances := make(map[string]bool)

	for _, instance := range storeInstances {
		if instance.ServiceName != service.Name {
			continue
		}
		// Skip Deleted instances and Failed *tombstones*. Tombstones
		// are preserved containers from a previous restart cycle kept
		// around for postmortem; the retention GC reclaims them
		// separately and they don't count against the desired scale.
		// Failed instances without a FailedAt timestamp are *not*
		// tombstones — they're transient Failed states from create/start
		// errors that the reconciler still needs to act on (replace,
		// surface in service status, etc.).
		if instance.Status == types.InstanceStatusDeleted {
			continue
		}
		if instance.Status == types.InstanceStatusFailed && instance.FailedAt != nil {
			continue
		}
		serviceInstances = append(serviceInstances, instance)
		serviceRunningInstances[instance.ID] = true
	}

	// Check for orphaned instances (running but not in store for this service).
	// Match on BOTH namespace and service name: a same-named service in another
	// namespace on this daemon (e.g. staging/api vs prod/api) is a different
	// service, and matching by name alone made each namespace reap the other's
	// live containers in a continuous loop. Namespace comes from the container's
	// rune.namespace label (docker runner List); if it's empty (pre-label
	// container) we leave the container alone rather than risk a cross-namespace
	// false positive.
	for instanceID, runningInst := range runningInstances {
		if serviceRunningInstances[instanceID] {
			continue
		}
		inst := runningInst.Instance
		if inst == nil {
			continue
		}
		if inst.Namespace != service.Namespace {
			continue // different (or unknown) namespace — not ours to reap
		}
		if inst.ServiceName != service.Name {
			continue
		}
		orphanedInstances = append(orphanedInstances, inst)
	}

	return &ServiceInstanceData{
		Instances:         serviceInstances,
		OrphanedInstances: orphanedInstances,
	}, nil
}

// scaleDownService removes excess instances when the desired scale is lower than current
func (r *Reconciler) scaleDownService(ctx context.Context, service *types.Service) error {
	// Get existing instances for this service
	instanceData, err := r.getServiceInstances(ctx, service)
	if err != nil {
		return err
	}

	if len(instanceData.Instances) <= service.Scale {
		return nil // No scaling down needed
	}

	r.logger.Info("Scaling down service",
		log.Str("service", service.Name),
		log.Int("current", len(instanceData.Instances)),
		log.Int("desired", service.Scale))

	// Sort instances by creation time (newest first). If zero timestamps in tests, sort by name for stability
	sort.Slice(instanceData.Instances, func(i, j int) bool {
		a := instanceData.Instances[i]
		b := instanceData.Instances[j]
		if !a.CreatedAt.IsZero() || !b.CreatedAt.IsZero() {
			return a.CreatedAt.After(b.CreatedAt)
		}
		// Fallback for tests where CreatedAt may be zero: sort by name ascending
		return a.Name < b.Name
	})

	// For now we'll use a simple approach removing from the end
	for i := service.Scale; i < len(instanceData.Instances); i++ {
		instance := instanceData.Instances[i]

		r.logger.Info("Removing excess instance",
			log.Str("service", service.Name),
			log.Str("instance", instance.ID))

		// Always remove from health monitoring
		// (regardless of current service.Health configuration)
		r.healthController.RemoveInstance(instance.ID)

		if err := r.instanceController.DeleteInstance(ctx, &instance); err != nil {
			r.logger.Error("Failed to remove excess instance",
				log.Str("instance", instance.ID),
				log.Err(err))
			// Continue with other instances even if one fails
		}
	}

	return nil
}

// reconcileExistingInstance updates an existing instance, recreating it if necessary
func (r *Reconciler) reconcileExistingInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	r.logger.Debug("Reconciling existing instance",
		log.Str("service", service.Name),
		log.Str("instance", instance.ID))

	// Stuck-in-create record: ContainerEverCreatedAt is nil so we
	// know the runner never accepted a container for this UUID. The
	// IsInstanceCompatibleWithService gate already routed it here
	// (the slot is held). Two branches:
	//
	//   * Status=Stalled       — retries exhausted. `rune restart <service>`
	//                            re-arms the slot; a cast will not, because
	//                            the stuck-in-create gate reports compatible
	//                            before any generation comparison. Do nothing
	//                            this tick.
	//   * Status=Failed        — backoff schedule controls the next
	//                            attempt. If NextCreateAttemptAt
	//                            has not yet passed, leave alone.
	//                            Otherwise call RetryCreateInstance
	//                            against the same UUID.
	//
	// This is the auto-retry counterpart to the loop-break gate
	// added in PR1; together they restore self-healing without the
	// UUID-confetti churn.
	if instance.ContainerEverCreatedAt == nil {
		switch instance.Status {
		case types.InstanceStatusStalled:
			// Operator action required; nothing to do.
			return nil
		case types.InstanceStatusFailed:
			if instance.NextCreateAttemptAt != nil && time.Now().Before(*instance.NextCreateAttemptAt) {
				// Backoff still in effect.
				return nil
			}
			if err := r.instanceController.RetryCreateInstance(ctx, service, instance); err != nil {
				r.logger.Warn("Retry create on stuck-in-create instance failed; backoff scheduled",
					log.Str("service", service.Name),
					log.Str("instance", instance.Name),
					log.Int("attempt", instance.CreateAttempts),
					log.Err(err))
			}
			return nil
		}
	}

	// Health monitoring must attach even when in-place update is not
	// yet possible (instance still Starting while the container boots).
	if service.Health != nil {
		if err := r.healthController.AddInstance(service, instance); err != nil {
			r.logger.Error("Failed to add instance to health monitoring",
				log.Str("instance", instance.ID),
				log.Err(err))
		}
	}

	// Try to update the instance in-place
	if err := r.instanceController.UpdateInstance(ctx, service, instance); err != nil {
		// Check if the error indicates that recreation is needed
		if r.isRecreationRequired(err) {
			return r.recreateInstance(ctx, service, instance)
		}
		// Some other update error occurred
		return fmt.Errorf("failed to update instance: %w", err)
	}

	return nil
}

// recreateInstance handles recreation of an instance that cannot be updated in-place
func (r *Reconciler) recreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	instanceName := instance.ID
	r.logger.Info("Instance requires recreation",
		log.Str("service", service.Name),
		log.Str("instance", instanceName))

	// First remove from health monitoring if applicable
	if service.Health != nil {
		r.healthController.RemoveInstance(instance.ID)
	}

	// Recreate the instance
	r.logger.Info("Recreating instance",
		log.Str("service", service.Name),
		log.Str("instance", instanceName))

	newInstance, err := r.instanceController.RecreateInstance(ctx, service, instance)
	if err != nil {
		return fmt.Errorf("failed to recreate instance: %w", err)
	}

	// Add the new instance to health monitoring if needed
	if service.Health != nil {
		if err := r.healthController.AddInstance(service, newInstance); err != nil {
			r.logger.Error("Failed to add recreated instance to health monitoring",
				log.Str("instance", instanceName),
				log.Err(err))
		}
	}

	return nil
}

// createNewInstance creates a new instance for a service. ordinal is the
// per-replica slot index, stored on the instance for stable volume binding.
func (r *Reconciler) createNewInstance(ctx context.Context, service *types.Service, instanceName string, ordinal int) error {
	r.logger.Info("Creating instance to achieve desired scale",
		log.Str("service", service.Name),
		log.Str("instance", instanceName))

	newInstance, err := r.instanceController.CreateInstance(ctx, service, instanceName, ordinal)
	if err != nil {
		return fmt.Errorf("failed to create instance: %w", err)
	}

	// Add the instance to health monitoring if applicable
	if service.Health != nil {
		if err := r.healthController.AddInstance(service, newInstance); err != nil {
			r.logger.Error("Failed to add instance to health monitoring",
				log.Str("instance", instanceName),
				log.Err(err))
			// Continue anyway
		}
	}

	return nil
}

// isRecreationRequired checks if an error from UpdateInstance indicates that
// the instance needs to be recreated rather than updated in-place
func (r *Reconciler) isRecreationRequired(err error) bool {
	if err == nil {
		return false
	}

	// Check for the specific error message pattern from UpdateInstance
	return strings.Contains(err.Error(), "requires recreation") ||
		strings.Contains(err.Error(), "incompatible") ||
		strings.Contains(err.Error(), "cannot be updated in-place")
}
