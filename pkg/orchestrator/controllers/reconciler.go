package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// The amount of time to keep deleted instances before removing them from store
const deletedInstanceRetentionTime = 10 * time.Minute

// The amount of time to wait before running garbage collection
const garbageCollectionInterval = 1 * time.Minute

// Defaults for the failed-instance retention GC. Kept here as constants for
// v1; the orchestrator can plumb config-driven overrides in a follow-up
// (runed.FailedInstanceRetention is already defined in internal/config).
const (
	// failedInstancePerServiceCap is the maximum number of tombstoned
	// Failed instances retained per service before the oldest is evicted.
	failedInstancePerServiceCap = 3
	// failedInstanceTTL is the maximum age of a tombstoned Failed instance
	// before it is evicted regardless of cap.
	failedInstanceTTL = 1 * time.Hour
)

// reconciler is responsible for ensuring the actual state of instances
// matches the desired state defined in the services
type reconciler struct {
	store              store.Store
	instanceController InstanceController
	healthController   HealthController
	logger             log.Logger
	reconcileInterval  time.Duration
	mu                 sync.Mutex
	isRunning          bool
	ctx                context.Context
	cancel             context.CancelFunc
	ticker             *time.Ticker
	wg                 sync.WaitGroup
}

// newReconciler creates a new reconciler.
func newReconciler(
	store store.Store,
	instanceController InstanceController,
	healthController HealthController,
	logger log.Logger,
) *reconciler {
	return &reconciler{
		store:              store,
		instanceController: instanceController,
		healthController:   healthController,
		logger:             logger.WithComponent("reconciler"),
		reconcileInterval:  30 * time.Second,
		mu:                 sync.Mutex{},
		wg:                 sync.WaitGroup{},
	}
}

// Start begins the periodic reconciliation loop
func (r *reconciler) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.isRunning {
		r.mu.Unlock()
		return nil // Already running, nothing to do
	}
	r.isRunning = true
	r.mu.Unlock()

	r.logger.Info("Starting reconciler")

	r.ctx, r.cancel = context.WithCancel(ctx)

	// Perform an initial reconciliation immediately
	if err := r.reconcileServices(r.ctx); err != nil {
		r.logger.Error("Initial reconciliation failed", log.Err(err))
		// Continue despite error as this will be retried by the ticker
	}

	// Start periodic reconciliation
	r.ticker = time.NewTicker(r.reconcileInterval)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		// Track the last time we ran garbage collection
		// Initially run after 5 minutes to ensure system is stable
		lastGC := time.Now().Add(-55 * time.Minute)

		for {
			select {
			case <-r.ctx.Done():
				return
			case <-r.ticker.C:
				r.logger.Debug("Running periodic reconciliation")

				// Reconcile all services
				if err := r.reconcileServices(r.ctx); err != nil {
					r.logger.Error("Reconciliation failed", log.Err(err))
				}

				// Clean up deleted services
				if err := r.handleDeletedServices(r.ctx); err != nil {
					r.logger.Error("Failed to clean up deleted services", log.Err(err))
				}

				// Run garbage collection roughly once per hour
				if time.Since(lastGC) > garbageCollectionInterval {
					if err := r.runGarbageCollection(r.ctx); err != nil {
						r.logger.Error("Garbage collection failed", log.Err(err))
					}
					lastGC = time.Now()
				}
			}
		}
	}()

	return nil
}

// Stop stops the reconciliation loop.
func (r *reconciler) Stop() {
	r.mu.Lock()
	if !r.isRunning {
		r.mu.Unlock()
		return // Not running, nothing to do
	}
	r.mu.Unlock()

	r.logger.Info("Stopping reconciler")

	// Cancel context to stop operations
	if r.cancel != nil {
		r.cancel()
	}

	// Stop the ticker
	if r.ticker != nil {
		r.ticker.Stop()
	}

	// Wait for goroutines to finish
	r.wg.Wait()

	// Mark as not running
	r.mu.Lock()
	r.isRunning = false
	r.mu.Unlock()

	r.logger.Info("Reconciler stopped")
}

// reconcileServices compares the desired state of services with the actual state
// and makes any necessary adjustments
func (r *reconciler) reconcileServices(ctx context.Context) error {
	r.logger.Debug("Starting service reconciliation")

	// Get desired state from store
	var services []types.Service
	if err := r.store.ListAll(ctx, types.ResourceTypeService, &services); err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	// Collect all orphaned instances across all services
	var allOrphanedInstances []*types.Instance

	// Reconcile each service's desired state with actual state
	for _, service := range services {
		if err := r.reconcileService(ctx, &service); err != nil {
			r.logger.Error("Failed to reconcile service",
				log.Str("service", service.Name),
				log.Str("namespace", service.Namespace),
				log.Err(err))
			// Continue with other services even if one fails
		}

		// Collect orphaned instances for this service
		instanceData, err := r.getServiceInstances(ctx, &service)
		if err != nil {
			r.logger.Error("Failed to get service instances for orphaned detection",
				log.Str("service", service.Name),
				log.Err(err))
			continue
		}
		allOrphanedInstances = append(allOrphanedInstances, instanceData.OrphanedInstances...)
	}

	r.logger.Debug("Completed reconciliation for services", log.Int("count", len(services)))

	// Handle orphaned instances (running but not in desired state)
	if err := r.cleanUpOrphanedInstances(ctx, allOrphanedInstances); err != nil {
		r.logger.Error("Failed to clean up orphaned instances", log.Err(err))
	}

	// Sweep store-orphan instances: any instance whose parent service
	// no longer exists in the store. The InstanceCleanupFinalizer
	// handles cascade-delete on the happy path, but live experience
	// (prod/tombstone-test-0 surviving 13h after its service was
	// deleted) shows the finalizer can miss instances when it
	// crashes mid-sweep, when the service was removed by a path that
	// bypassed the finalizer, or when a new instance is created
	// against a service ID that has since gone. This is the safety
	// net so `rune delete <service>` never leaves a long-lived
	// orphan that operators can't address — they show up in
	// `rune get instances` with a serviceName pointing at nothing.
	var allInstances []types.Instance
	if err := r.store.ListAll(ctx, types.ResourceTypeInstance, &allInstances); err != nil {
		r.logger.Error("Failed to list instances for orphan sweep", log.Err(err))
	} else {
		// Re-list services AFTER the instances so the "known" set is never
		// staler than the instance list. `services` above is snapshotted at the
		// top of the cycle; a service created mid-cycle (cast creates the
		// service, then its instance) would otherwise be absent from that
		// snapshot while its just-created instance appears in allInstances — and
		// the sweep would delete a healthy, in-flight workload (RUNE cast race).
		var freshServices []types.Service
		if err := r.store.ListAll(ctx, types.ResourceTypeService, &freshServices); err != nil {
			r.logger.Error("Failed to list services for orphan sweep", log.Err(err))
		} else {
			r.cleanUpStoreOrphanInstances(ctx, freshServices, allInstances)
		}
	}

	r.logger.Debug("Service reconciliation completed")
	return nil
}

// storeOrphanGrace is how long after creation an instance is immune from the
// store-orphan sweep. It must comfortably exceed one reconcile interval so an
// instance created in the same cycle as its service is never mistaken for an
// orphan before the service becomes visible to the sweep.
const storeOrphanGrace = 90 * time.Second

// cleanUpStoreOrphanInstances deletes instances whose parent service
// (by namespace + name) is no longer in the store. Pure function over
// the supplied snapshot of services + instances — the caller owns
// the store I/O so this is straightforward to unit-test.
func (r *reconciler) cleanUpStoreOrphanInstances(ctx context.Context, knownServices []types.Service, instances []types.Instance) {
	known := make(map[string]struct{}, len(knownServices))
	for i := range knownServices {
		known[knownServices[i].Namespace+"/"+knownServices[i].Name] = struct{}{}
	}

	for i := range instances {
		inst := &instances[i]
		// Already on the way out — let the existing deleted-instance
		// retention path finish its job. Terminating means a
		// teardown is mid-flight (runner.Stop/Remove in progress);
		// hitting DeleteInstance again would just re-enter the same
		// idempotent flow and clutter the audit trail.
		if inst.Status == types.InstanceStatusDeleted ||
			inst.Status == types.InstanceStatusTerminating {
			continue
		}
		// ServiceName is the contract: the InstanceCleanupFinalizer,
		// reconciler slot lookup, and rune CLI all key off it. An
		// empty value would be a separate bug; skip rather than mass-
		// delete on the assumption that the finalizer would have
		// caught real instances anyway.
		if inst.ServiceName == "" {
			continue
		}
		// Grace window: never sweep a freshly-created instance. `cast` writes the
		// service then the instance, and a reconcile cycle can list this instance
		// before its parent service is visible to *this* sweep's service snapshot
		// — deleting it would tear down a healthy, still-starting workload. A
		// genuine orphan (service really gone) is older than one reconcile
		// interval and gets swept on a later pass, so nothing leaks; we only
		// refuse to act while the create is plausibly still in flight.
		if !inst.CreatedAt.IsZero() && time.Since(inst.CreatedAt) < storeOrphanGrace {
			continue
		}
		if _, ok := known[inst.Namespace+"/"+inst.ServiceName]; ok {
			continue
		}
		r.logger.Info("Cleaning up store-orphan instance (parent service no longer exists)",
			log.Str("instance", inst.ID),
			log.Str("service", inst.ServiceName),
			log.Str("namespace", inst.Namespace),
			log.Str("status", string(inst.Status)))
		if err := r.instanceController.DeleteInstance(ctx, inst); err != nil {
			// Log and continue; missing one orphan shouldn't block the
			// rest of the sweep.
			r.logger.Error("Failed to delete store-orphan instance",
				log.Str("instance", inst.ID),
				log.Err(err))
			continue
		}
		// The per-service VolumeCleanupFinalizer normally reclaims a stateful
		// instance's claimTemplate volume, but it can't run here: the parent
		// service is already gone (that's why this instance is an orphan). Do
		// it inline or the volume leaks — left Available, bound to a deleted
		// instance, with its backing store (e.g. a DO block volume) still
		// provisioned.
		r.reclaimOrphanInstanceVolumes(ctx, inst)
	}
}

// reclaimOrphanInstanceVolumes deletes the claimTemplate volume(s) bound to a
// store-orphan instance, mirroring the VolumeCleanupFinalizer's per-volume
// steps: unbind (so the agent's volume subsystem tears down Mount/Attach) then
// delete the owned row (so the VolumeController's delete handler runs the
// reclaim policy on the backing store). Keyed off the instance's own resolved
// VolumeMounts (authoritative) and guarded by OwnerService so operator-managed
// `claim:` volumes — which outlive any one service — are never touched. Best-
// effort; logged, never fatal.
func (r *reconciler) reclaimOrphanInstanceVolumes(ctx context.Context, inst *types.Instance) {
	if inst.ServiceName == "" || inst.Metadata == nil || len(inst.Metadata.VolumeMounts) == 0 {
		return
	}
	owner := inst.Namespace + "/" + inst.ServiceName

	for _, m := range inst.Metadata.VolumeMounts {
		if m.VolumeName == "" {
			continue
		}
		var v types.Volume
		if err := r.store.Get(ctx, types.ResourceTypeVolume, inst.Namespace, m.VolumeName, &v); err != nil {
			// Already gone (or unreadable) — nothing to reclaim.
			continue
		}
		// Only claimTemplate-owned volumes are reclaimed when their owning
		// service vanishes; operator-managed `claim:` volumes (no OwnerService)
		// are independent of any one service's lifetime.
		if v.OwnerService != owner {
			continue
		}
		// Unbind first so the agent Unmounts/Detaches before the row is gone.
		if v.BoundNode != "" || v.BoundClaim != "" {
			v.BoundNode = ""
			v.BoundClaim = ""
			if v.Handle != "" {
				v.Status = types.VolumeStatusAvailable
			}
			if err := r.store.Update(ctx, types.ResourceTypeVolume, v.Namespace, v.Name, &v); err != nil {
				r.logger.Warn("orphan volume reclaim: failed to unbind volume",
					log.Str("volume", v.Name), log.Err(err))
			}
		}
		if err := r.store.Delete(ctx, types.ResourceTypeVolume, v.Namespace, v.Name); err != nil {
			r.logger.Warn("orphan volume reclaim: failed to delete volume row",
				log.Str("volume", v.Name), log.Err(err))
			continue
		}
		r.logger.Info("Reclaimed claimTemplate volume of store-orphan instance",
			log.Str("volume", v.Name),
			log.Str("instance", inst.ID),
			log.Str("namespace", v.Namespace))
	}
}

func (r *reconciler) cleanUpOrphanedInstances(ctx context.Context, orphanedInstances []*types.Instance) error {
	for _, instance := range orphanedInstances {
		if instance != nil {
			r.logger.Info("Cleaning up orphaned instance",
				log.Str("instance", instance.ID),
				log.Str("service", instance.ServiceID))
			if err := r.instanceController.DeleteInstance(ctx, instance); err != nil {
				r.logger.Error("Failed to clean up orphaned instance",
					log.Str("instance", instance.ID),
					log.Err(err))
				return err
			}

		}
	}
	return nil
}

// reconcileService ensures that a single service's instances match the desired state
func (r *reconciler) reconcileService(ctx context.Context, service *types.Service) error {
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

	// Skip reconciliation if the service is marked as deleted
	if service.Status == types.ServiceStatusDeleted {
		r.logger.Debug("Skipping reconciliation for deleted service",
			log.Str("service", service.Name),
			log.Str("namespace", service.Namespace))
		return nil
	}

	// Scale down if needed
	if err := r.scaleDownService(ctx, service); err != nil {
		r.logger.Error("Error scaling down service",
			log.Str("service", service.Name),
			log.Err(err))
		// Continue with the rest of reconciliation
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

// reconcileSingleService reconciles a single service with running instances collection
// This is used by the service controller for immediate reconciliation on service events
func (r *reconciler) reconcileSingleService(ctx context.Context, service *types.Service) error {
	r.logger.Debug("Reconciling single service",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace))

	// Reconcile the service using the existing logic
	return r.reconcileService(ctx, service)
}

// getServiceInstances retrieves and filters instances for a specific service
// Marks running instances that belong to this service as not orphaned
func (r *reconciler) getServiceInstances(ctx context.Context, service *types.Service) (*ServiceInstanceData, error) {
	// Get running instances from all runners
	runningInstances, err := r.instanceController.collectRunningInstances(ctx)
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

	// Check for orphaned instances (running but not in store for this service)
	for instanceID, runningInst := range runningInstances {
		if !serviceRunningInstances[instanceID] {
			// This instance is running but not in our service's store instances
			// Check if it belongs to this service
			if runningInst.Instance != nil && runningInst.Instance.ServiceName == service.Name {
				orphanedInstances = append(orphanedInstances, runningInst.Instance)
			}
		}
	}

	return &ServiceInstanceData{
		Instances:         serviceInstances,
		OrphanedInstances: orphanedInstances,
	}, nil
}

// scaleDownService removes excess instances when the desired scale is lower than current
func (r *reconciler) scaleDownService(ctx context.Context, service *types.Service) error {
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

// ensureServiceInstances makes sure we have the right number of instances and they're up to date
func (r *reconciler) ensureServiceInstances(ctx context.Context, service *types.Service) error {
	// If the service declares dependencies, gate instance creation until deps are ready
	if len(service.Dependencies) > 0 {
		ready, err := r.dependenciesReady(ctx, service)
		if err != nil {
			r.logger.Error("Dependency readiness check failed",
				log.Str("service", service.Name),
				log.Err(err))
			// Be safe: do not proceed with instance creation on error
			return nil
		}
		if !ready {
			r.logger.Info("Delaying instance creation; dependencies not ready",
				log.Str("service", service.Name),
				log.Str("namespace", service.Namespace))
			return nil
		}
	}

	// Get existing instances for this service
	instanceData, err := r.getServiceInstances(ctx, service)
	if err != nil {
		return err
	}
	r.logger.Debug("Ensuring service instances",
		log.Str("service", service.Name),
		log.Int("desired", service.Scale),
		log.Int("current", len(instanceData.Instances)))

	// Stateful services (per-replica volume claimTemplates) keep stable
	// {service}-{ordinal} slot names so replicas rebind their volumes across
	// restarts; stateless services get unique {service}-{shorthash} names per
	// lifetime (#84). The two need different reconcile identity models — slot
	// matching vs. count-based — so dispatch here.
	if serviceHasStableIdentity(service) {
		return r.ensureStatefulInstances(ctx, service, instanceData)
	}
	return r.ensureStatelessInstances(ctx, service, instanceData)
}

// ensureStatefulInstances reconciles a service whose replicas have stable
// ordinal identity. For each slot 0..Scale-1 it looks up the {service}-{ordinal}
// instance by name: a compatible one is reconciled in place, an incompatible one
// is deleted and recreated in the same slot, and a missing one is created. This
// is the StatefulSet-style model where the name *is* the slot key.
func (r *reconciler) ensureStatefulInstances(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData) error {
	for i := 0; i < service.Scale; i++ {
		// Generate instance name
		instanceName := generateInstanceName(service, i)

		// Check if this instance already exists and is compatible
		var existingInstance *types.Instance
		for j := range instanceData.Instances {
			if instanceData.Instances[j].Name == instanceName {
				// Use the existing compatibility check function
				isCompatible, reason := r.instanceController.isInstanceCompatibleWithService(ctx, &instanceData.Instances[j], service)
				if isCompatible {
					existingInstance = &instanceData.Instances[j]
					break
				} else {
					r.logger.Info("Instance incompatible, will recreate",
						log.Str("service", service.Name),
						log.Str("instance", instanceName),
						log.Str("reason", reason))
					// Always remove the old instance from health monitoring
					// (regardless of current service.Health configuration)
					r.healthController.RemoveInstance(instanceData.Instances[j].ID)
					// Delete the old instance
					if err := r.instanceController.DeleteInstance(ctx, &instanceData.Instances[j]); err != nil {
						r.logger.Error("Failed to delete old instance during recreation",
							log.Str("instance", instanceData.Instances[j].ID),
							log.Err(err))
					}
				}
			}
		}

		if existingInstance != nil {
			// Update existing instance
			if err := r.reconcileExistingInstance(ctx, service, existingInstance); err != nil {
				r.logger.Error("Failed to reconcile existing instance",
					log.Str("service", service.Name),
					log.Str("instance", instanceName),
					log.Err(err))
				// Continue with other instances
			}
			continue
		}

		r.logger.Info("creating new instance", log.Json("instanceName", instanceName))
		// Create a new instance — i is the per-replica slot ordinal.
		if err := r.createNewInstance(ctx, service, instanceName, i); err != nil {
			r.logger.Error("Failed to create new instance",
				log.Str("service", service.Name),
				log.Str("instance", instanceName),
				log.Err(err))
			// Continue with other instances
		}
	}

	return nil
}

// ensureStatelessInstances reconciles a service whose replicas have no stable
// identity. Instance names are unique per lifetime, so they can't be regenerated
// from a slot index; identity is by count instead. Compatible instances are
// reconciled in place and kept; incompatible ones are deleted; then enough
// fresh {service}-{shorthash} instances are created to reach the desired scale.
// Removing *excess* instances is handled separately by scaleDownService (which
// runs before this in reconcileService), so this only ever creates.
func (r *reconciler) ensureStatelessInstances(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData) error {
	taken := make(map[string]bool, len(instanceData.Instances))
	have := 0
	for j := range instanceData.Instances {
		inst := &instanceData.Instances[j]
		isCompatible, reason := r.instanceController.isInstanceCompatibleWithService(ctx, inst, service)
		if !isCompatible {
			r.logger.Info("Instance incompatible, will recreate",
				log.Str("service", service.Name),
				log.Str("instance", inst.Name),
				log.Str("reason", reason))
			r.healthController.RemoveInstance(inst.ID)
			if err := r.instanceController.DeleteInstance(ctx, inst); err != nil {
				r.logger.Error("Failed to delete old instance during recreation",
					log.Str("instance", inst.ID),
					log.Err(err))
			}
			continue
		}
		if err := r.reconcileExistingInstance(ctx, service, inst); err != nil {
			r.logger.Error("Failed to reconcile existing instance",
				log.Str("service", service.Name),
				log.Str("instance", inst.Name),
				log.Err(err))
			// Continue with other instances
		}
		taken[inst.Name] = true
		have++
	}

	// Create fresh instances up to the desired scale. Ordinal is not meaningful
	// for a stateless service (no per-replica volume binding); pass the running
	// index purely so the field is populated deterministically.
	for ordinal := have; ordinal < service.Scale; ordinal++ {
		instanceName := generateHashInstanceName(service, taken)
		taken[instanceName] = true
		r.logger.Info("creating new instance", log.Json("instanceName", instanceName))
		if err := r.createNewInstance(ctx, service, instanceName, ordinal); err != nil {
			r.logger.Error("Failed to create new instance",
				log.Str("service", service.Name),
				log.Str("instance", instanceName),
				log.Err(err))
			// Continue with other instances
		}
	}

	return nil
}

// dependenciesReady evaluates whether all declared dependencies for a service are ready.
// Readiness definition (MVP):
// - If dependency is not a service, it's ready
// - If dependency is a service and it defines readiness probe: at least one instance is Running and readiness=true
// - Else: at least one instance is Running
func (r *reconciler) dependenciesReady(ctx context.Context, service *types.Service) (bool, error) {
	for _, dep := range service.Dependencies {
		// Fetch dependency service
		dependencyResource, err := r.fetchDependencyResource(ctx, &dep, service)
		if err != nil {
			return false, err
		}

		// If dependency is not a service, it's ready
		depService, ok := dependencyResource.(types.Service)
		if !ok {
			continue
		}

		// List instances for dependency
		instances, err := r.listInstancesForService(ctx, depService.Namespace, dep.Service)
		if err != nil {
			return false, fmt.Errorf("failed to list instances for dependency %s/%s: %w", depService.Namespace, dep.Service, err)
		}

		// Evaluate readiness
		readyFound := false
		hasReadinessProbe := depService.Health != nil && depService.Health.Readiness != nil
		for _, inst := range instances {
			if inst.Status != types.InstanceStatusRunning {
				continue
			}
			if !hasReadinessProbe {
				// Running instance is sufficient
				readyFound = true
				break
			}
			// Check readiness via health controller
			status, err := r.healthController.GetHealthStatus(ctx, inst.ID)
			if err != nil {
				// On error, treat as not ready; continue
				continue
			}
			if status != nil && status.Readiness {
				readyFound = true
				break
			}
		}
		if !readyFound {
			return false, nil
		}
	}
	return true, nil
}

func (r *reconciler) fetchDependencyResource(ctx context.Context, dep *types.DependencyRef, service *types.Service) (interface{}, error) {
	var dependencyResource interface{}
	depNS := dep.Namespace
	if depNS == "" {
		depNS = service.Namespace
	}

	depResourceType := dep.GetDependencyResourceType()
	depResourceName := dep.GetDependencyResourceName()

	// For dependencies, fetch into concrete types so the store can unmarshal correctly
	switch depResourceType {
	case types.ResourceTypeService:
		var svc types.Service
		// Use explicit service name if provided, else fall back to computed name
		name := dep.Service
		if name == "" {
			name = depResourceName
		}
		if err := r.store.Get(ctx, types.ResourceTypeService, depNS, name, &svc); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, name, err)
		}
		return svc, nil
	case types.ResourceTypeConfigmap:
		var cfg types.Configmap
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &cfg); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return cfg, nil
	case types.ResourceTypeSecret:
		var sec types.Secret
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &sec); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return sec, nil
	default:
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &dependencyResource); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return dependencyResource, nil
	}
}

// reconcileExistingInstance updates an existing instance, recreating it if necessary
func (r *reconciler) reconcileExistingInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	r.logger.Debug("Reconciling existing instance",
		log.Str("service", service.Name),
		log.Str("instance", instance.ID))

	// Stuck-in-create record: ContainerEverCreatedAt is nil so we
	// know the runner never accepted a container for this UUID. The
	// isInstanceCompatibleWithService gate already routed it here
	// (the slot is held). Two branches:
	//
	//   * Status=Stalled       — retries exhausted; operator must
	//                            run `rune restart instance` or
	//                            `rune cast` (new generation) to
	//                            re-arm. Do nothing this tick.
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
func (r *reconciler) recreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
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
func (r *reconciler) createNewInstance(ctx context.Context, service *types.Service, instanceName string, ordinal int) error {
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

// updateServiceStatus updates a service's status based on its instances
func (r *reconciler) updateServiceStatus(ctx context.Context, service *types.Service) error {
	// Don't change the status if service is already marked as deleted
	if service.Status == types.ServiceStatusDeleted {
		return nil
	}

	// Fetch the latest instance data directly from the store
	instanceData, err := r.getServiceInstances(ctx, service)
	if err != nil {
		return fmt.Errorf("failed to get latest instances for status update: %w", err)
	}

	var newStatus types.ServiceStatus
	var newReason, newMessage string

	if len(instanceData.Instances) == 0 {
		// No instances yet
		newStatus = types.ServiceStatusPending
	} else {
		// Count instances in each state
		pending := 0
		running := 0
		failed := 0

		// Track the worst instance so we can roll its StatusMessage up
		// to the service. Failed wins over Pending wins over Running.
		// First failed instance encountered is enough — operators get
		// one concrete sentence to act on.
		var worstFailed, worstPending *types.Instance

		for i := range instanceData.Instances {
			instance := &instanceData.Instances[i]
			switch instance.Status {
			case types.InstanceStatusPending, types.InstanceStatusCreated, types.InstanceStatusStarting:
				pending++
				if worstPending == nil {
					worstPending = instance
				}
			case types.InstanceStatusRunning:
				running++
			case types.InstanceStatusFailed, types.InstanceStatusExited, types.InstanceStatusUnknown, types.InstanceStatusStalled:
				// Stalled is the PR2 terminal "create retries
				// exhausted, operator must act" state. Roll it up as
				// Failed so `rune get services` doesn't show
				// misleading Pending for a slot that will never
				// self-recover.
				failed++
				if worstFailed == nil {
					worstFailed = instance
				}
			}
		}

		r.logger.Debug("Instance status counts",
			log.Str("service", service.Name),
			log.Int("pending", pending),
			log.Int("running", running),
			log.Int("failed", failed),
			log.Int("total", len(instanceData.Instances)))

		// Determine overall service status.
		//
		// Stopping wins over Running/Deploying/Pending whenever the desired
		// scale is below the current instance count — the reconciler is
		// actively tearing instances down. Without this, `rune stop` and the
		// drain phase of `rune restart` show "Running" the whole time the
		// old instance is still around, which reads as a contradiction next
		// to the desired scale of 0.
		//
		// Failed still wins over Stopping: an instance that's failing to
		// terminate cleanly is more important to surface than the fact that
		// we're trying to terminate it.
		switch {
		case failed > 0:
			newStatus = types.ServiceStatusFailed
			if worstFailed != nil {
				newReason = types.DeriveServiceReason(worstFailed.Status, worstFailed.StatusMessage)
				newMessage = worstFailed.StatusMessage
			}
		case service.Scale < len(instanceData.Instances):
			newStatus = types.ServiceStatusStopping
		case pending > 0:
			newStatus = types.ServiceStatusDeploying
			if worstPending != nil && worstPending.StatusMessage != "" {
				newMessage = worstPending.StatusMessage
			}
		case running == len(instanceData.Instances):
			newStatus = types.ServiceStatusRunning
		default:
			newStatus = types.ServiceStatusPending
		}
	}

	// The generation we just reconciled. Recording it in ObservedGeneration is
	// how the service controller later recognises a status-only update and skips
	// a redundant reconcile (RFC #129 Phase 2). Capture the snapshot generation,
	// not the fresh one — if the desired state changed again mid-reconcile, the
	// mismatch must persist so the next event reconciles the newer generation.
	reconciledGen := int64(0)
	if service.Metadata != nil {
		reconciledGen = service.Metadata.Generation
	}

	statusChanged := service.Status != newStatus ||
		service.StatusReason != newReason ||
		service.StatusMessage != newMessage
	// Even when the status text is unchanged, we must advance ObservedGeneration
	// so a scale-up that leaves the service "Running" (e.g. `rune restart`'s
	// scale-back-up) doesn't look unreconciled forever and reconcile-loop.
	genNeedsObserve := service.Metadata == nil || service.Metadata.ObservedGeneration != reconciledGen

	if statusChanged || genNeedsObserve {
		if statusChanged {
			r.logger.Info("Updating service status",
				log.Str("service", service.Name),
				log.Str("from", string(service.Status)),
				log.Str("to", string(newStatus)),
				log.Str("reason", newReason))
		}

		// Apply ONLY the status fields (+ ObservedGeneration), atomically, on the
		// freshly-read service. updateServiceStatus runs at the end of a reconcile
		// cycle on a `service` snapshot taken at the top; a concurrent scale write
		// (e.g. `rune restart`'s 0→1 leg via the scaling controller) can land in
		// between. UpdateFunc's CAS guarantees this write never clobbers that
		// Scale (the "restart goes 1→0 and hangs" bug) (RFC #129).
		var fresh types.Service
		err := r.store.UpdateFunc(ctx, types.ResourceTypeService, service.Namespace, service.Name, &fresh, func() error {
			// Don't resurrect a service that was deleted out from under us.
			if fresh.Status == types.ServiceStatusDeleted {
				return store.ErrSkipUpdate
			}
			if fresh.Metadata == nil {
				fresh.Metadata = &types.ServiceMetadata{}
			}
			// Skip only if BOTH the status and the observed generation already
			// match — otherwise another writer beat us but we still owe the
			// ObservedGeneration bump (or vice versa).
			statusMatches := fresh.Status == newStatus &&
				fresh.StatusReason == newReason &&
				fresh.StatusMessage == newMessage
			if statusMatches && fresh.Metadata.ObservedGeneration == reconciledGen {
				return store.ErrSkipUpdate
			}
			fresh.Status = newStatus
			fresh.StatusReason = newReason
			fresh.StatusMessage = newMessage
			fresh.Metadata.ObservedGeneration = reconciledGen
			return nil
		}, store.WithReconciler())
		if err != nil {
			return fmt.Errorf("failed to update service status: %w", err)
		}
		// Reflect the persisted fields on the caller's copy for consistency.
		service.Status = newStatus
		service.StatusReason = newReason
		service.StatusMessage = newMessage
		if service.Metadata == nil {
			service.Metadata = &types.ServiceMetadata{}
		}
		service.Metadata.ObservedGeneration = reconciledGen
	}

	return nil
}

// ServiceInstanceData contains instances and orphaned instance information
type ServiceInstanceData struct {
	Instances         []types.Instance
	OrphanedInstances []*types.Instance // Actual orphaned instance objects
}

// RunningInstance represents an instance found running in a runner
type RunningInstance struct {
	Instance   *types.Instance
	IsOrphaned bool
	Runner     types.RunnerType
}

// handleDeletedServices cleans up services that are marked as deleted
func (r *reconciler) handleDeletedServices(ctx context.Context) error {
	// Get all services from store
	var services []types.Service
	err := r.store.ListAll(ctx, types.ResourceTypeService, &services)
	if err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	// Check for services marked as deleted
	for _, service := range services {
		// If service is marked as deleted, remove it from the store
		if service.Status == types.ServiceStatusDeleted {
			// Check if all instances have been cleaned up
			instances, err := r.listInstancesForService(ctx, service.Namespace, service.Name)
			if err != nil {
				r.logger.Error("Failed to list instances for deleted service",
					log.Str("name", service.Name),
					log.Str("namespace", service.Namespace),
					log.Err(err))
				continue
			}

			if len(instances) == 0 {
				// All instances are gone, we can remove the service from the store
				r.logger.Info("Removing deleted service from store",
					log.Str("name", service.Name),
					log.Str("namespace", service.Namespace))

				if err := r.store.Delete(ctx, types.ResourceTypeService, service.Namespace, service.Name); err != nil {
					r.logger.Error("Failed to remove deleted service from store",
						log.Str("name", service.Name),
						log.Str("namespace", service.Namespace),
						log.Err(err))
				}
			}
		}
	}

	return nil
}

// listInstancesForService lists all instances for a service
func (r *reconciler) listInstancesForService(ctx context.Context, namespace, serviceName string) ([]types.Instance, error) {
	// Get all instances
	var instances []types.Instance
	err := r.store.List(ctx, types.ResourceTypeInstance, namespace, &instances)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	// Filter instances for this service (by ServiceName)
	filtered := make([]types.Instance, 0, len(instances))
	for _, instance := range instances {
		if instance.ServiceName == serviceName {
			filtered = append(filtered, instance)
		}
	}

	return filtered, nil
}

// isRecreationRequired checks if an error from UpdateInstance indicates that
// the instance needs to be recreated rather than updated in-place
func (r *reconciler) isRecreationRequired(err error) bool {
	if err == nil {
		return false
	}

	// Check for the specific error message pattern from UpdateInstance
	return strings.Contains(err.Error(), "requires recreation") ||
		strings.Contains(err.Error(), "incompatible") ||
		strings.Contains(err.Error(), "cannot be updated in-place")
}

// runGarbageCollection removes instances that have been marked as deleted
// after a specified retention period has passed
func (r *reconciler) runGarbageCollection(ctx context.Context) error {
	r.logger.Debug("Running garbage collection")

	// Get all instances
	var instances []types.Instance
	err := r.store.ListAll(ctx, types.ResourceTypeInstance, &instances)
	if err != nil {
		return fmt.Errorf("failed to list instances for garbage collection: %w", err)
	}

	// Failed-instance retention pass. Done before the deleted-instance pass
	// below because we want any tombstones whose TTL has expired to be
	// promoted to Deleted so the existing deleted-instance retention can
	// keep cleaning them out of the store.
	r.gcFailedInstances(ctx, instances)

	// Filter for deleted instances
	for _, instance := range instances {
		if instance.Status == types.InstanceStatusDeleted && instance.Metadata != nil {
			// Check if retention period has passed
			if instance.Metadata.DeletionTimestamp == nil {
				// No timestamp, keep it for now and log the issue
				r.logger.Warn("Found deleted instance without deletion timestamp",
					log.Str("instance", instance.ID),
					log.Str("namespace", instance.Namespace))
				continue
			}

			// Parse the timestamp
			deletedAt := *instance.Metadata.DeletionTimestamp

			// Check if retention period has passed
			if time.Since(deletedAt) > deletedInstanceRetentionTime {
				r.logger.Info("Garbage collecting deleted instance",
					log.Str("instance", instance.ID),
					log.Str("service", instance.ServiceName),
					log.Str("namespace", instance.Namespace),
					log.Str("deletedAt", deletedAt.Format(time.RFC3339)),
					log.Str("age", time.Since(deletedAt).String()))

				// Remove from store
				if err := r.store.Delete(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID); err != nil {
					r.logger.Error("Failed to garbage collect instance",
						log.Str("instance", instance.ID),
						log.Err(err))
				} else {
					r.logger.Info("Successfully garbage collected instance",
						log.Str("instance", instance.ID))
				}
			}
		}
	}

	return nil
}

// gcFailedInstances evicts tombstoned Failed instances per the retention
// policy: per-service cap and TTL, whichever fires first. Eviction
// transitions a Failed instance to Deleted (with a DeletionTimestamp) so the
// existing deleted-instance retention sweep above picks it up on a later
// tick. Removing the underlying container is handled by
// instanceController.DeleteInstance.
func (r *reconciler) gcFailedInstances(ctx context.Context, instances []types.Instance) {
	now := time.Now()

	// Bucket Failed-and-still-preserved instances by service.
	bySvc := map[string][]*types.Instance{}
	for i := range instances {
		inst := &instances[i]
		if inst.Status != types.InstanceStatusFailed || inst.FailedAt == nil {
			continue
		}
		key := inst.Namespace + "/" + inst.ServiceName
		bySvc[key] = append(bySvc[key], inst)
	}

	for key, tombs := range bySvc {
		// Sort logs-bearing first, then newest-first within each
		// group. The cap walks from index 0 and evicts beyond
		// per-service-cap, so this preferentially KEEPS tombstones
		// that have a captured stdout/stderr snapshot — exactly the
		// ones operators want for postmortems. Live observation:
		// prod/gateway's one informative crash (f67e328f, 14KB) was
		// previously evicted by the same cap that swept 6 silent
		// tombstones; with this ordering it survives until the TTL
		// fires (which is still respected below as a hard ceiling).
		sort.Slice(tombs, func(i, j int) bool {
			ihas := len(tombs[i].LastLogs) > 0
			jhas := len(tombs[j].LastLogs) > 0
			if ihas != jhas {
				return ihas
			}
			return tombs[i].FailedAt.After(*tombs[j].FailedAt)
		})

		for i, t := range tombs {
			tooOld := failedInstanceTTL > 0 && now.Sub(*t.FailedAt) > failedInstanceTTL
			beyondCap := failedInstancePerServiceCap > 0 && i >= failedInstancePerServiceCap
			if !tooOld && !beyondCap {
				continue
			}

			reason := "ttl"
			if beyondCap && !tooOld {
				reason = "cap"
			}
			r.logger.Info("Evicting failed-instance tombstone",
				log.Str("service", key),
				log.Str("tombstone_instance", t.ID),
				log.Str("container_id", t.ContainerID),
				log.Str("age", now.Sub(*t.FailedAt).String()),
				log.Str("reason", reason))

			// DeleteInstance stops + removes the container (idempotent
			// against an already-stopped container) and marks the store
			// record Deleted with a DeletionTimestamp. The deleted-
			// instance retention sweep cleans the row a few minutes later.
			if err := r.instanceController.DeleteInstance(ctx, t); err != nil {
				r.logger.Warn("Failed to evict failed-instance tombstone",
					log.Str("tombstone_instance", t.ID),
					log.Err(err))
			}
		}
	}
}
