package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/queue"
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

// reconcileWorkerCount is how many queue workers reconcile services in
// parallel. Per-key exclusivity is guaranteed by the queue regardless of this
// number — two workers can never reconcile the same service concurrently — so
// this only bounds cross-service parallelism.
const reconcileWorkerCount = 4

// reconciler ensures the actual state of instances matches the desired state
// defined in the services. All per-service reconciliation is serialized
// through a coalescing per-key workqueue (RFC #129 Phase 3): watch events and
// the periodic resync enqueue "namespace/name" keys, and queue workers run
// syncService — so reconciles of one service can never interleave, making the
// reconciler the single orchestrator-side writer of Service status.
// reconcilerInstanceOps is the slice of the instance controller the
// reconciler drives — lifecycle plus classification (RUNE-311 Phase 3).
// The consumer owns the interface; *InstanceController satisfies it.
type reconcilerInstanceOps interface {
	CreateInstance(ctx context.Context, service *types.Service, instanceName string, ordinal int) (*types.Instance, error)
	RetryCreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error
	RecreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) (*types.Instance, error)
	UpdateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error
	StopInstance(ctx context.Context, instance *types.Instance) error
	DeleteInstance(ctx context.Context, instance *types.Instance) error
	WithdrawServiceInstances(ctx context.Context, service *types.Service, instances []*types.Instance)
	RepublishService(ctx context.Context, service *types.Service)
	classifyInstance(ctx context.Context, instance *types.Instance, service *types.Service) CompatVerdict
	collectRunningInstances(ctx context.Context) (map[string]*RunningInstance, error)
	isInstanceCompatibleWithService(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string)
}

type reconciler struct {
	store              store.Store
	instanceController reconcilerInstanceOps
	healthController   HealthController
	logger             log.Logger

	// events is the optional persisted event log. Set after construction via
	// SetEventLog; nil-safe, so unit tests need not wire one. Update
	// lifecycle events go here — they are the only after-the-fact record of
	// what a rolling update did, since Service.Update is cleared on
	// completion (RUNE-042 §8.2).
	events            events.EventLog
	reconcileInterval time.Duration
	queue             *queue.Queue
	mu                sync.Mutex
	isRunning         bool
	ctx               context.Context
	cancel            context.CancelFunc
	ticker            *time.Ticker
	wg                sync.WaitGroup
}

// newReconciler creates a new reconciler.
func newReconciler(
	store store.Store,
	instanceController reconcilerInstanceOps,
	healthController HealthController,
	logger log.Logger,
) *reconciler {
	return &reconciler{
		store:              store,
		instanceController: instanceController,
		healthController:   healthController,
		logger:             logger.WithComponent("reconciler"),
		reconcileInterval:  30 * time.Second,
		queue:              queue.New("service-reconcile", queue.DefaultRateLimiter()),
		mu:                 sync.Mutex{},
		wg:                 sync.WaitGroup{},
	}
}

// Enqueue schedules a reconcile of one service. Safe to call from any
// goroutine; duplicate enqueues coalesce and a service being reconciled right
// now is re-run exactly once afterwards.
func (r *reconciler) Enqueue(namespace, name string) {
	r.queue.Add(namespace + "/" + name)
}

// enqueueAllServices schedules a reconcile for every service in the store —
// the periodic level-triggered resync that catches anything a lost or filtered
// event would otherwise leave behind.
func (r *reconciler) enqueueAllServices(ctx context.Context) error {
	var services []types.Service
	if err := r.store.ListAll(ctx, types.ResourceTypeService, &services); err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}
	for i := range services {
		r.Enqueue(services[i].Namespace, services[i].Name)
	}
	return nil
}

// syncService is the queue worker handler: reconcile one service by key.
// A nil return means the key is settled (including "service deleted"); an
// error requeues the key with backoff.
func (r *reconciler) syncService(ctx context.Context, key string) error {
	namespace, name, ok := strings.Cut(key, "/")
	if !ok {
		r.logger.Error("Dropping malformed reconcile key", log.Str("key", key))
		return nil // malformed keys can't succeed on retry
	}

	var service types.Service
	if err := r.store.Get(ctx, types.ResourceTypeService, namespace, name, &service); err != nil {
		if store.IsNotFoundError(err) {
			// Deleted between enqueue and sync — finalizers and housekeeping
			// own the cleanup; nothing to reconcile.
			return nil
		}
		return fmt.Errorf("failed to get service %s: %w", key, err)
	}

	return r.reconcileService(ctx, &service)
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

	// Start the reconcile workers. queue.Work blocks until the context is
	// cancelled (which shuts the queue down), so it runs under the WaitGroup
	// and Stop's wg.Wait() covers worker teardown.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.queue.Work(r.ctx, reconcileWorkerCount, r.syncService)
	}()

	// Seed an initial full reconcile immediately (level-triggered baseline).
	if err := r.enqueueAllServices(r.ctx); err != nil {
		r.logger.Error("Initial reconcile enqueue failed", log.Err(err))
		// Continue despite error as this will be retried by the ticker
	}
	r.runHousekeeping(r.ctx)

	// Start the periodic resync: re-enqueue every service (safety net for
	// anything event-driven flow missed) and run the cross-service sweeps.
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
				// Queue observability: one Debug line per resync tick.
				stats := r.queue.Stats()
				r.logger.Debug("Running periodic resync",
					log.Int("queue_depth", stats.Depth),
					log.Int("queue_max_depth", stats.MaxDepth),
					log.Any("adds", stats.Adds),
					log.Any("coalesced", stats.Coalesced),
					log.Any("requeues", stats.Requeues),
					log.Any("processed", stats.Processed),
					log.Str("work_duration_total", stats.WorkDurationTotal.String()))

				// Re-enqueue all services for reconciliation
				if err := r.enqueueAllServices(r.ctx); err != nil {
					r.logger.Error("Resync enqueue failed", log.Err(err))
				}

				// Runner-orphan sweep (runtime drift). Deleted-service cleanup
				// is no longer a sweep: reconcileDeletion (Phase 4) removes the
				// record as the terminal step of the per-service teardown.
				r.runHousekeeping(r.ctx)

				// Run garbage collection when due
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

	// Shut down the workqueue (idempotent; queue.Work also does this on
	// context cancellation) so blocked workers wake up and exit.
	r.queue.ShutDown()

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

// runHousekeeping performs the one remaining cross-service sweep: reaping
// RUNNER-orphaned instances — instances a runner reports running that are
// absent from any service's desired state (runtime drift, e.g. a container
// that outlived its record). Runs on the periodic resync tick.
//
// The STORE-orphan sweep (instances whose parent service is gone) + its 90s
// grace window + inline volume reclaim were retired in RFC #129 Phase 4b:
// foreground deletion (reconcileDeletion) removes the service record only
// AFTER its instances and owned volumes are gone, so a store-orphan instance
// (or a leaked claimTemplate volume) can no longer exist by construction.
func (r *reconciler) runHousekeeping(ctx context.Context) {
	r.logger.Debug("Starting housekeeping sweep")

	var services []types.Service
	if err := r.store.ListAll(ctx, types.ResourceTypeService, &services); err != nil {
		r.logger.Error("Failed to list services for housekeeping", log.Err(err))
		return
	}

	// Collect all orphaned instances across all services (instances found
	// running in a runner but absent from the desired state).
	var allOrphanedInstances []*types.Instance
	for i := range services {
		instanceData, err := r.getServiceInstances(ctx, &services[i])
		if err != nil {
			r.logger.Error("Failed to get service instances for orphaned detection",
				log.Str("service", services[i].Name),
				log.Err(err))
			continue
		}
		allOrphanedInstances = append(allOrphanedInstances, instanceData.OrphanedInstances...)
	}

	// Handle orphaned instances (running but not in desired state)
	if err := r.cleanUpOrphanedInstances(ctx, allOrphanedInstances); err != nil {
		r.logger.Error("Failed to clean up orphaned instances", log.Err(err))
	}

	r.logger.Debug("Housekeeping sweep completed")
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
	// If the service declares dependencies, gate instance CREATION until they
	// are ready — creation only, not the whole pass.
	//
	// This used to return early, which was harmless while scale-down ran
	// ahead of it in reconcileService. Now that stateless excess removal
	// lives in the plan, an early return also suppresses retirement: a
	// service whose dependency went unready would ignore `rune scale`
	// entirely, keeping every instance up while the operator watched the
	// command time out. Blocking creates while still honouring the desired
	// scale downward is both what the log line claims and the safer half.
	createsBlocked := false
	if len(service.Dependencies) > 0 {
		ready, err := r.dependenciesReady(ctx, service)
		if err != nil {
			r.logger.Error("Dependency readiness check failed",
				log.Str("service", service.Name),
				log.Err(err))
			// Be safe: do not create against an unknown dependency state.
			createsBlocked = true
		} else if !ready {
			r.logger.Info("Delaying instance creation; dependencies not ready",
				log.Str("service", service.Name),
				log.Str("namespace", service.Namespace))
			createsBlocked = true
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
		// The stateful path only ever creates or replaces in-slot, so the
		// old all-or-nothing gate is still the right shape for it.
		if createsBlocked {
			return nil
		}
		return r.ensureStatefulInstances(ctx, service, instanceData)
	}
	return r.ensureStatelessInstances(ctx, service, instanceData, createsBlocked)
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
// identity, through the update planner (RUNE-042 Phase 4).
//
// Before this, the loop deleted EVERY incompatible instance and then created
// replacements — which is why a template change took the whole service down
// at once. Now each instance is classified, the planner decides what may
// happen this tick within the availability budget, and this function only
// executes that decision.
//
// Scale-down is part of the same decision (the planner returns excess
// retirements), so there is no separate scale-down pass to fight the surge.
func (r *reconciler) ensureStatelessInstances(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData, createsBlocked bool) error {
	// Finish any teardown that was abandoned mid-flight before planning, or
	// the stranded record occupies a slot no plan can free.
	r.reapStuckTerminating(ctx, service, instanceData)

	plan, views := r.planServiceUpdate(ctx, service, instanceData)

	// 1. Retire, oldest/least-valuable first as the planner ordered them.
	//
	// Withdraw the whole set from the dataplane in one publish first, and
	// take ONE shared drain window for all of them (RUNE-042 §4). Retiring
	// serially would pay a full drain per instance — 8 × (5s drain + up to
	// 10s stop) ≈ two minutes of one of only four reconcile workers for a
	// `recreate` deploy or a wide scale-down, during which this service
	// creates nothing and other services wait. The per-instance
	// DeleteInstance calls below then see Terminating and skip their own
	// drains.
	if len(plan.Retire) > 1 {
		r.instanceController.WithdrawServiceInstances(ctx, service, plan.Retire)
	}
	for _, inst := range plan.Retire {
		r.logger.Info("Retiring instance",
			log.Str("service", service.Name),
			log.Str("instance", inst.Name),
			log.Str("reason", plan.Reason))
		r.emitService(ctx, service, types.EventLevelInfo, eventInstanceRetired,
			fmt.Sprintf("retired %s: %s", inst.Name, plan.Reason))
		r.healthController.RemoveInstance(inst.ID)
		if err := r.instanceController.DeleteInstance(ctx, inst); err != nil {
			r.logger.Error("Failed to retire instance",
				log.Str("instance", inst.ID), log.Err(err))
		}
	}

	// 2. Repair broken instances — unbudgeted, because they serve nobody.
	retired := make(map[string]bool, len(plan.Retire))
	for _, inst := range plan.Retire {
		retired[inst.ID] = true
	}
	for _, inst := range plan.Repair {
		r.logger.Info("Replacing broken instance",
			log.Str("service", service.Name),
			log.Str("instance", inst.Name))
		r.healthController.RemoveInstance(inst.ID)
		if err := r.instanceController.DeleteInstance(ctx, inst); err != nil {
			r.logger.Error("Failed to remove broken instance",
				log.Str("instance", inst.ID), log.Err(err))
		}
	}

	// 3. Reconcile the survivors in place (health monitoring, env drift,
	//    generation stamp). UpdateInstance leaves outdated instances alone —
	//    their replacement is the planner's call, made above.
	taken := make(map[string]bool, len(views))
	for i := range views {
		inst := views[i].Instance
		if retired[inst.ID] || views[i].Class == CompatBroken {
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
	}

	// 4. Create what the plan allows: replacements for retired/broken
	//    instances plus any shortfall against the desired scale. Ordinal is
	//    not meaningful for a stateless service (no per-replica volume
	//    binding); pass the running index so the field is populated
	//    deterministically.
	total := len(plan.Repair) + plan.Create
	if createsBlocked && total > 0 {
		r.logger.Info("Holding instance creation; dependencies not ready",
			log.Str("service", service.Name),
			log.Int("would_create", total))
		total = 0
	}
	for i := 0; i < total; i++ {
		instanceName := generateHashInstanceName(service, taken)
		taken[instanceName] = true
		r.logger.Info("creating new instance", log.Json("instanceName", instanceName))
		if err := r.createNewInstance(ctx, service, instanceName, len(taken)-1); err != nil {
			r.logger.Error("Failed to create new instance",
				log.Str("service", service.Name),
				log.Str("instance", instanceName),
				log.Err(err))
			// Continue with other instances
		}
	}

	return nil
}

// planServiceUpdate classifies the service's instances and asks the planner
// what may happen this tick. Returns the plan and the classified views, which
// the caller reuses so nothing is classified twice (each classification hits
// the runner).
func (r *reconciler) planServiceUpdate(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData) (updatePlan, []instanceView) {
	params := service.ResolveUpdateParams()
	now := time.Now()

	views := make([]instanceView, 0, len(instanceData.Instances))
	for i := range instanceData.Instances {
		inst := &instanceData.Instances[i]
		verdict := r.instanceController.classifyInstance(ctx, inst, service)
		v := newInstanceView(inst, verdict.Class, params.MinReady, now)
		if verdict.Class != CompatOK {
			r.logger.Debug("Instance classified",
				log.Str("service", service.Name),
				log.Str("instance", inst.Name),
				log.Str("class", classNames[verdict.Class]),
				log.Str("reason", verdict.Reason))
		}
		views = append(views, v)
	}

	return planUpdate(updateInput{
		Scale:     service.Scale,
		Params:    params,
		Instances: views,
		Now:       now,
	}), views
}

// classNames renders a CompatClass for logs.
var classNames = map[CompatClass]string{
	CompatOK:       "ok",
	CompatBroken:   "broken",
	CompatOutdated: "outdated",
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
	var upd *types.UpdateStatus
	var stalled bool

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

		// Update progress + stall detection (RUNE-042 §8.2/§7.3). nil means
		// no update is in flight.
		upd, stalled = r.computeUpdateStatus(ctx, service, instanceData)

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
		//
		// Two of these rules break under an update and are guarded (RUNE-042
		// §8.2):
		//
		//   - An update legitimately runs at Scale+extra instances, so the
		//     count-exceeds-scale test would report the whole update as
		//     "Stopping" — wrong, and alarming next to a healthy deploy.
		//   - One replacement failing to start must not flip a service whose
		//     old instances are all serving. While an update is in flight a
		//     failed instance keeps the service Deploying/Updating until the
		//     stall deadline, and only then becomes Failed/UpdateStalled.
		//     A failed instance with NO update in flight keeps the old
		//     behaviour exactly.
		updating := upd != nil
		switch {
		case failed > 0 && (!updating || stalled):
			newStatus = types.ServiceStatusFailed
			if stalled {
				newReason = types.ServiceReasonUpdateStalled
				newMessage = stalledMessage(upd)
			} else if worstFailed != nil {
				newReason = types.DeriveServiceReason(worstFailed.Status, worstFailed.StatusMessage)
				newMessage = worstFailed.StatusMessage
			}
		case stalled:
			newStatus = types.ServiceStatusFailed
			newReason = types.ServiceReasonUpdateStalled
			newMessage = stalledMessage(upd)
		case updating:
			newStatus = types.ServiceStatusDeploying
			newReason = types.ServiceReasonUpdating
			newMessage = upd.Message
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
		service.StatusMessage != newMessage ||
		updateStatusChanged(service.Update, upd)
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
				log.Str("reason", newReason),
				// Without this the planner's actual sentence ("waiting for
				// replacements to become ready") never reached the log, and a
				// held update was undiagnosable from logs alone.
				log.Str("message", newMessage))
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
				fresh.StatusMessage == newMessage &&
				!updateStatusChanged(fresh.Update, upd)
			if statusMatches && fresh.Metadata.ObservedGeneration == reconciledGen {
				return store.ErrSkipUpdate
			}
			fresh.Status = newStatus
			fresh.StatusReason = newReason
			fresh.StatusMessage = newMessage
			// Update rides this same CAS write. A separate write would
			// reintroduce exactly the clobber-a-concurrent-scale race RFC #129
			// closed. nil clears it — an update that has converged leaves no
			// stale block behind, because Verify and the CLI spinner both key
			// off the transition back to Running.
			fresh.Update = upd
			fresh.Metadata.ObservedGeneration = reconciledGen
			return nil
		}, store.WithReconciler())
		if err != nil {
			return fmt.Errorf("failed to update service status: %w", err)
		}
		r.emitUpdateTransitions(ctx, service, service.Update, upd, stalled)

		// Reflect the persisted fields on the caller's copy for consistency.
		// Clone Metadata rather than writing through it: the pointer may be
		// shared with watch-event consumers and other store readers
		// (TestStore's copies are shallow; Badger round-trips, but the
		// discipline holds either way) — an in-place write would race with
		// their reads. The top-level status fields are our own struct copy.
		service.Status = newStatus
		service.StatusReason = newReason
		service.StatusMessage = newMessage
		service.Update = upd
		md := types.ServiceMetadata{}
		if service.Metadata != nil {
			md = *service.Metadata
		}
		md.ObservedGeneration = reconciledGen
		service.Metadata = &md
	}

	return nil
}

// reapStuckTerminating re-drives instances stranded in Terminating.
//
// Teardown flips an instance to Terminating, publishes the withdrawal, drains,
// then stops and marks it Deleted. If runed is killed — or a cancellable
// request context is cut, e.g. Ctrl-C on a command that triggered the
// teardown — inside that window, the record is left Terminating with a live
// container and nothing ever revisits it: the planner excludes Terminating
// from every candidate list, so it can be neither retired nor replaced, and
// the service runs permanently below scale.
//
// Re-entering DeleteInstance is idempotent (it tolerates an already-stopped or
// already-absent container), so the repair is simply to finish the teardown.
func (r *reconciler) reapStuckTerminating(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData) {
	// Generous: the drain plus the runner stop timeout plus slack. Anything
	// older than this is not a teardown in flight, it is an abandoned one.
	deadline := service.DrainWindow() + 30*time.Second
	now := time.Now()

	for i := range instanceData.Instances {
		inst := &instanceData.Instances[i]
		if inst.Status != types.InstanceStatusTerminating {
			continue
		}
		stuckFor := now.Sub(inst.UpdatedAt)
		if stuckFor < deadline {
			continue // a teardown genuinely in progress
		}
		r.logger.Warn("Re-driving instance stranded in Terminating",
			log.Str("service", service.Name),
			log.Str("instance", inst.Name),
			log.Duration("stuck_for", stuckFor))
		r.healthController.RemoveInstance(inst.ID)
		if err := r.instanceController.DeleteInstance(ctx, inst); err != nil {
			r.logger.Error("Failed to re-drive stranded instance",
				log.Str("instance", inst.ID), log.Err(err))
		}
	}
}

// computeUpdateStatus derives the in-flight update block for a service, and
// reports whether the update has stalled (RUNE-042 §7.3/§8.2).
//
// Returns (nil, false) when no update is running — every live instance is at
// the current template — which is what clears Service.Update on completion.
//
// Progress is measured against the PERSISTED previous block, so LastProgressAt
// survives a runed restart mid-update and the stall clock is not reset by one.
// A dependency gate pauses the clock rather than letting it expire: an update
// frozen because a dependency is unready is a healthy hold, not a stall.
func (r *reconciler) computeUpdateStatus(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData) (*types.UpdateStatus, bool) {
	params := service.ResolveUpdateParams()
	now := time.Now()

	var templateGen int64
	if service.Metadata != nil {
		templateGen = service.Metadata.TemplateGeneration
	}

	next := &types.UpdateStatus{
		TemplateGeneration: templateGen,
		Desired:            service.Scale,
	}
	for i := range instanceData.Instances {
		inst := &instanceData.Instances[i]
		verdict := r.instanceController.classifyInstance(ctx, inst, service)
		v := newInstanceView(inst, verdict.Class, params.MinReady, now)

		if verdict.Class == CompatOutdated && !v.Terminating {
			// Mirror the planner, which excludes Terminating instances from
			// liveOutdated. Counting one here without the planner being able
			// to retire it means Outdated can never reach zero: the update
			// reads as permanently in flight, never progresses, and lands on
			// UpdateStalled forever — with every later `release --atomic` on
			// the service failing verify. A record stranded in Terminating
			// (runed killed mid-teardown) is reaped by reapStuckTerminating
			// instead.
			next.Outdated++
		} else if verdict.Class == CompatOK {
			// "Updated" counts instances at the current template. A repaired
			// instance lands there too — CreateInstance stamps the current
			// TemplateGeneration — so converging through crash-replacements
			// registers as progress rather than reading as a permanent stall
			// (§8.2).
			next.Updated++
			if v.Ready {
				next.UpdatedReady++
			}
		}
		if v.serving() {
			next.Available++
		}
	}

	// No outdated instances left: the update is done (or never started).
	if next.Outdated == 0 {
		return nil, false
	}

	prev := service.Update
	next.StartedAt = now
	next.LastProgressAt = now
	if prev != nil && prev.TemplateGeneration == templateGen {
		next.StartedAt = prev.StartedAt
		next.LastProgressAt = prev.LastProgressAt
		// Ask BEFORE raising the marks, or every tick would trivially match
		// its own peak.
		if prev.Progressed(next) {
			next.LastProgressAt = now
		}
	}
	next.CarryPeaksFrom(prev)

	// A dependency gate freezes instance creation entirely, so the stall
	// clock would expire on a service that is behaving correctly.
	if len(service.Dependencies) > 0 {
		if ready, err := r.dependenciesReady(ctx, service); err == nil && !ready {
			next.LastProgressAt = now
			next.Message = "blocked on dependency"
			return next, false
		}
	}

	// The planner's own sentence is the most useful thing to show ("waiting
	// for replacements to become ready", "retiring 1 instance(s)"), so reuse
	// it rather than inventing a second vocabulary.
	if next.Message == "" {
		views := make([]instanceView, 0, len(instanceData.Instances))
		for i := range instanceData.Instances {
			inst := &instanceData.Instances[i]
			verdict := r.instanceController.classifyInstance(ctx, inst, service)
			views = append(views, newInstanceView(inst, verdict.Class, params.MinReady, now))
		}
		plan := planUpdate(updateInput{Scale: service.Scale, Params: params, Instances: views, Now: now})
		next.Message = plan.Reason
	}

	stalled := now.Sub(next.LastProgressAt) > params.StallDeadline
	return next, stalled
}

// stalledMessage explains a stall without throwing away the planner's own
// sentence, which is the part that says WHY nothing is moving.
func stalledMessage(upd *types.UpdateStatus) string {
	base := "update made no progress within the stall deadline"
	if upd == nil || upd.Message == "" {
		return base
	}
	return base + ": " + upd.Message
}

// updateStatusChanged reports whether the update block needs persisting.
// Timestamps are deliberately excluded from the comparison except when they
// carry news: LastProgressAt moves only on real progress, so comparing the
// counters plus the message avoids a store write on every reconcile tick of a
// held update.
func updateStatusChanged(a, b *types.UpdateStatus) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil || b == nil:
		return true
	}
	return a.TemplateGeneration != b.TemplateGeneration ||
		a.Desired != b.Desired ||
		a.Updated != b.Updated ||
		a.UpdatedReady != b.UpdatedReady ||
		a.Available != b.Available ||
		a.Outdated != b.Outdated ||
		a.Message != b.Message ||
		!a.LastProgressAt.Equal(b.LastProgressAt)
}

// SetEventLog wires the persisted event log. Nil-safe.
func (r *reconciler) SetEventLog(eventLog events.EventLog) { r.events = eventLog }

// Well-known event reasons for the update lifecycle. Kept in the "update"
// vocabulary the spec field uses — operators read `updateStrategy`, so they
// should not have to learn a second word for the same thing.
const (
	eventUpdateStarted   = "UpdateStarted"
	eventInstanceRetired = "InstanceRetired"
	eventUpdateHolding   = "UpdateHolding"
	eventUpdateStalled   = "UpdateStalled"
	eventUpdateComplete  = "UpdateComplete"
)

// emitService records a service-scoped event. Best-effort and nil-safe.
func (r *reconciler) emitService(ctx context.Context, service *types.Service, level types.EventLevel, reason, message string) {
	if r.events == nil || service == nil {
		return
	}
	if err := r.events.Emit(ctx, types.Event{
		Namespace: service.Namespace,
		Kind:      "Service",
		Name:      service.Name,
		UID:       service.ID,
		Level:     level,
		Reason:    reason,
		Message:   message,
	}); err != nil {
		r.logger.Warn("Failed to emit service event",
			log.Str("service", service.Name), log.Err(err))
	}
}

// emitUpdateTransitions records the update lifecycle by comparing the block we
// are about to persist against the one already stored. Only transitions are
// emitted — a held update ticks every 30s and must not produce an event each
// time, which is the difference between a readable timeline and log spam.
func (r *reconciler) emitUpdateTransitions(ctx context.Context, service *types.Service, prev, next *types.UpdateStatus, stalled bool) {
	if r.events == nil {
		return
	}

	switch {
	case prev == nil && next != nil:
		r.emitService(ctx, service, types.EventLevelInfo, eventUpdateStarted,
			fmt.Sprintf("updating %d instance(s) to template generation %d",
				next.Outdated, next.TemplateGeneration))

	case prev != nil && next == nil:
		r.emitService(ctx, service, types.EventLevelInfo, eventUpdateComplete,
			fmt.Sprintf("update to template generation %d complete; %d instance(s) serving",
				prev.TemplateGeneration, prev.Available))

	case prev != nil && next != nil:
		// A new template landed mid-update: report the old one finishing and
		// the new one starting rather than silently switching targets.
		if prev.TemplateGeneration != next.TemplateGeneration {
			r.emitService(ctx, service, types.EventLevelInfo, eventUpdateStarted,
				fmt.Sprintf("superseded by template generation %d; %d instance(s) outdated",
					next.TemplateGeneration, next.Outdated))
			return
		}
		if stalled && service.StatusReason != types.ServiceReasonUpdateStalled {
			// Edge-triggered: only when the service is crossing INTO stalled.
			r.emitService(ctx, service, types.EventLevelWarn, eventUpdateStalled,
				fmt.Sprintf("no progress for %s: %s",
					time.Since(next.LastProgressAt).Round(time.Second), next.Message))
			return
		}
		// Holding is edge-triggered on the message changing, so a steady hold
		// is recorded once rather than on every resync tick.
		if !stalled && next.Message != "" && next.Message != prev.Message {
			r.emitService(ctx, service, types.EventLevelInfo, eventUpdateHolding, next.Message)
		}
	}
}

// ServiceInstanceData contains instances and orphaned instance information// ServiceInstanceData contains instances and orphaned instance information
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
