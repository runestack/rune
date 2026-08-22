package reconciler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/health"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
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
// InstanceOps is the slice of the instance controller the
// reconciler drives — lifecycle plus classification (RUNE-311 Phase 3).
// The consumer owns the interface; *InstanceController satisfies it.
type InstanceOps interface {
	CreateInstance(ctx context.Context, service *types.Service, instanceName string, ordinal int) (*types.Instance, error)
	RetryCreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error
	RecreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) (*types.Instance, error)
	UpdateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error
	StopInstance(ctx context.Context, instance *types.Instance) error
	DeleteInstance(ctx context.Context, instance *types.Instance) error
	WithdrawServiceInstances(ctx context.Context, service *types.Service, instances []*types.Instance)
	RepublishService(ctx context.Context, service *types.Service)
	ClassifyInstance(ctx context.Context, instance *types.Instance, service *types.Service) instancectl.CompatVerdict
	CollectRunningInstances(ctx context.Context) (map[string]*instancectl.RunningInstance, error)
	IsInstanceCompatibleWithService(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string)
}

type Reconciler struct {
	store              store.Store
	instanceController InstanceOps
	healthController   health.Controller
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

// New creates a new reconciler.
func New(
	store store.Store,
	instanceController InstanceOps,
	healthController health.Controller,
	logger log.Logger,
) *Reconciler {
	return &Reconciler{
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
func (r *Reconciler) Enqueue(namespace, name string) {
	r.queue.Add(namespace + "/" + name)
}

// enqueueAllServices schedules a reconcile for every service in the store —
// the periodic level-triggered resync that catches anything a lost or filtered
// event would otherwise leave behind.
func (r *Reconciler) enqueueAllServices(ctx context.Context) error {
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
func (r *Reconciler) syncService(ctx context.Context, key string) error {
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
func (r *Reconciler) Start(ctx context.Context) error {
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
func (r *Reconciler) Stop() {
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

// ServiceInstanceData contains instances and orphaned instance information// ServiceInstanceData contains instances and orphaned instance information
type ServiceInstanceData struct {
	Instances         []types.Instance
	OrphanedInstances []*types.Instance // Actual orphaned instance objects
}

// listInstancesForService lists all instances for a service
func (r *Reconciler) listInstancesForService(ctx context.Context, namespace, serviceName string) ([]types.Instance, error) {
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

// Compile-time proof the real controller satisfies this consumer's slice.
var _ InstanceOps = (*instancectl.Controller)(nil)

// QueueLen reports the number of pending reconcile keys. Test-facing:
// the service controller's enqueue-filtering tests assert on it.
func (r *Reconciler) QueueLen() int { return r.queue.Len() }

// SyncService reconciles one "namespace/name" key immediately, on the
// caller's goroutine. TEST SEAM ONLY: it bypasses the per-key
// single-writer workqueue (RFC #129 Phase 3), so a production caller
// would let reconciles of one service interleave — the exact race class
// the workqueue exists to prevent. Production flow goes through Enqueue.
func (r *Reconciler) SyncService(ctx context.Context, key string) error {
	return r.syncService(ctx, key)
}

// EnqueueAll schedules a reconcile for every service in the store —
// the level-triggered resync.
func (r *Reconciler) EnqueueAll(ctx context.Context) error {
	return r.enqueueAllServices(ctx)
}

// SetReconcileInterval overrides the periodic resync interval. Call
// before Start; test seam.
func (r *Reconciler) SetReconcileInterval(d time.Duration) { r.reconcileInterval = d }
