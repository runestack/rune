package controllers

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/finalizers"
	"github.com/runestack/rune/pkg/orchestrator/tasks"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"github.com/runestack/rune/pkg/worker/pool"
)

// ServiceController implements the Controller interface for service management
type ServiceController interface {
	Start(ctx context.Context) error
	Stop() error
	GetServiceStatus(ctx context.Context, namespace, name string) (*types.ServiceStatusInfo, error)
	UpdateServiceStatus(ctx context.Context, service *types.Service, status types.ServiceStatus) error
	GetServiceLogs(ctx context.Context, namespace, name string, opts types.LogOptions) (io.ReadCloser, error)
	ExecInService(ctx context.Context, namespace, serviceName string, options types.ExecOptions) (types.ExecStream, error)
	DialInService(ctx context.Context, namespace, serviceName string, port uint32) (net.Conn, *types.Instance, error)
	RestartService(ctx context.Context, namespace, serviceName string) error
	StopService(ctx context.Context, namespace, serviceName string) error
	DeleteService(ctx context.Context, request *types.DeletionRequest) (*types.DeletionResponse, error)
	GetDeletionStatus(ctx context.Context, namespace, name string) (*types.DeletionOperation, error)
	listInstancesForService(ctx context.Context, namespace, serviceName string) ([]*types.Instance, error)
	processServiceEvent(ctx context.Context, event store.WatchEvent) error
	handleServiceDeleted(ctx context.Context, service *types.Service) error
	handleServiceCreated(ctx context.Context, service *types.Service) error
	handleServiceUpdated(ctx context.Context, service *types.Service) error
}

// serviceController implements the ServiceController interface
type serviceController struct {
	store              store.Store
	instanceController InstanceController
	healthController   HealthController
	scalingController  ScalingController
	logger             log.Logger

	// Reconciliation system
	reconciler *reconciler

	// Context for background operations
	ctx    context.Context
	cancel context.CancelFunc

	// WaitGroup for goroutines
	wg sync.WaitGroup

	// Watch channel for services
	watchCh <-chan store.WatchEvent

	// For tracking observed generations to prevent reconciliation loops
	serviceObservedGenerations map[string]int64
	// Observed desired scale per service. Scale changes are driven by the
	// ScalingController without bumping Generation, so a scale-only update would
	// otherwise look like a status-only change and be skipped — leaving new
	// replicas (and `rune restart`'s scale-back-up) uncreated until the next
	// 30s reconcile tick. Tracking the last reconciled scale lets
	// isStatusOnlyChange detect those and reconcile promptly.
	serviceObservedScales map[string]int
	serviceObservedLock   sync.RWMutex

	// Deletion system dependencies
	deletionWorkerPool *pool.WorkerPool
	finalizerExecutor  *finalizers.FinalizerExecutor
}

// NewServiceController creates a new service controller
func NewServiceController(
	store store.Store,
	instanceController InstanceController,
	healthController HealthController,
	scalingController ScalingController,
	logger log.Logger,
) (ServiceController, error) {
	// Create the reconciler once
	reconciler := newReconciler(
		store,
		instanceController,
		healthController,
		logger.WithComponent("service-reconciler"),
	)

	// Create worker pool for deletion tasks
	deletionWorkerPool, err := pool.DeletionWorkerPool(3)
	if err != nil {
		return nil, fmt.Errorf("failed to create deletion worker pool: %w", err)
	}

	// Create finalizer executor
	finalizerExecutor := finalizers.DefaultFinalizerExecutor(
		store,
		instanceController,
		healthController,
		logger,
	)

	return &serviceController{
		store:                      store,
		finalizerExecutor:          finalizerExecutor,
		deletionWorkerPool:         deletionWorkerPool,
		instanceController:         instanceController,
		healthController:           healthController,
		scalingController:          scalingController,
		reconciler:                 reconciler,
		logger:                     logger.WithComponent("service-controller"),
		serviceObservedGenerations: make(map[string]int64),
		serviceObservedScales:      make(map[string]int),
	}, nil
}

// Start starts the service controller
func (sc *serviceController) Start(ctx context.Context) error {
	sc.logger.Info("Starting service controller")

	// Create a context with cancel for all background operations
	sc.ctx, sc.cancel = context.WithCancel(ctx)

	// Start the deletion worker pool
	sc.deletionWorkerPool.Start()
	sc.logger.Info("Started deletion worker pool")

	// Recover deletion operations interrupted by a crash/restart (mirrors scaling recovery).
	if err := sc.recoverInProgressDeletions(ctx); err != nil {
		sc.logger.Error("Error recovering in-progress deletion operations", log.Err(err))
	}

	// Start watching for service events
	if err := sc.StartWatching(ctx); err != nil {
		return fmt.Errorf("failed to start watching: %w", err)
	}

	// Start periodic reconciliation (safety net)
	if err := sc.StartPeriodicReconciliation(sc.ctx); err != nil {
		sc.logger.Error("Failed to start periodic reconciliation", log.Err(err))
		return err
	}

	sc.logger.Info("Service controller started")
	return nil
}

// Stop stops the service controller
func (sc *serviceController) Stop() error {
	sc.logger.Info("Stopping service controller")

	// Cancel context to stop all operations
	if sc.cancel != nil {
		sc.cancel()
	}

	// Stop the deletion worker pool
	sc.deletionWorkerPool.Stop()
	sc.logger.Info("Stopped deletion worker pool")

	// Stop watching for service events
	if err := sc.StopWatching(); err != nil {
		sc.logger.Error("Failed to stop watching", log.Err(err))
	}

	// Stop periodic reconciliation
	if err := sc.StopPeriodicReconciliation(); err != nil {
		sc.logger.Error("Failed to stop periodic reconciliation", log.Err(err))
	}

	// Wait for all goroutines to finish
	sc.wg.Wait()

	sc.logger.Info("Service controller stopped")
	return nil
}

// Controller interface implementation

func (sc *serviceController) Name() string {
	return "service-controller"
}

func (sc *serviceController) StartWatchers(ctx context.Context) error {
	sc.logger.Info("Starting service watchers")
	return sc.StartWatching(ctx)
}

func (sc *serviceController) StopWatchers() error {
	sc.logger.Info("Stopping service watchers")
	return sc.StopWatching()
}

func (sc *serviceController) StartPeriodicReconciliation(ctx context.Context) error {
	sc.logger.Info("Starting periodic reconciliation")

	// Use the reconciler's built-in Start method which handles the ticker properly
	return sc.reconciler.Start(ctx)
}

func (sc *serviceController) StopPeriodicReconciliation() error {
	sc.logger.Info("Stopping periodic reconciliation")

	// Use the reconciler's built-in Stop method which handles the ticker properly
	sc.reconciler.Stop()
	return nil
}

func (sc *serviceController) handleServiceCreated(ctx context.Context, service *types.Service) error {
	sc.logger.Info("Service created",
		log.Str("name", service.Name),
		log.Str("namespace", service.Namespace))

	// Set initial service state
	service.Status = types.ServiceStatusPending

	// Update service status in store
	if err := sc.UpdateServiceStatus(ctx, service, types.ServiceStatusPending); err != nil {
		sc.logger.Error("Failed to update service status",
			log.Str("name", service.Name),
			log.Str("namespace", service.Namespace),
			log.Err(err))
		return err
	}

	// Trigger immediate reconciliation to create instances
	if err := sc.reconciler.reconcileSingleService(ctx, service); err != nil {
		sc.logger.Error("Failed to reconcile service after creation",
			log.Str("name", service.Name),
			log.Str("namespace", service.Namespace),
			log.Err(err))
		return err
	}

	return nil
}

func (sc *serviceController) handleServiceUpdated(ctx context.Context, service *types.Service) error {
	sc.logger.Info("Service updated",
		log.Str("name", service.Name),
		log.Str("namespace", service.Namespace))

	// Check if this is a status-only change
	if sc.isStatusOnlyChange(service) {
		sc.logger.Debug("Skipping reconciliation for status-only change",
			log.Str("name", service.Name),
			log.Str("namespace", service.Namespace),
			log.Int64("generation", service.Metadata.Generation))
		return nil
	}

	// If there's an active scaling operation for this service, skip direct reconciliation here
	// and let the ScalingController drive updates to service.Scale.
	if op, _ := sc.scalingController.GetActiveOperation(ctx, service.Namespace, service.Name); op != nil {
		sc.logger.Debug("Skipping direct reconciliation due to active scaling operation",
			log.Str("name", service.Name),
			log.Str("namespace", service.Namespace))
	} else {
		// Trigger immediate reconciliation to handle the update
		if err := sc.reconciler.reconcileSingleService(ctx, service); err != nil {
			sc.logger.Error("Failed to reconcile service after update",
				log.Str("name", service.Name),
				log.Str("namespace", service.Namespace),
				log.Err(err))
			return err
		}
		// Record the scale we just reconciled so a later status-only update at
		// the same scale is correctly skipped. Recorded only after an actual
		// reconcile (not in the active-scaling-op skip branch) so a recorded
		// scale always reflects a reconciled state.
		sc.recordObservedScale(service.Namespace, service.Name, service.Scale)
	}

	// Record the observed generation
	sc.recordObservedGeneration(service.Namespace, service.Name, service.Metadata.Generation)

	return nil
}

func (sc *serviceController) handleServiceDeleted(ctx context.Context, service *types.Service) error {
	sc.logger.Info("Service deleted event received",
		log.Str("name", service.Name),
		log.Str("namespace", service.Namespace))

	// Service is already deleted, finalizers handle cleanup
	// No additional work needed
	return nil
}

func (sc *serviceController) GetServiceStatus(ctx context.Context, namespace, name string) (*types.ServiceStatusInfo, error) {
	// Get service from store
	var service types.Service
	if err := sc.store.Get(ctx, types.ResourceTypeService, namespace, name, &service); err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}

	// List instances for this service
	instances, err := sc.listInstancesForService(ctx, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	observedGeneration, exists := sc.getObservedGeneration(namespace, name)
	if !exists {
		observedGeneration = 0
	}

	status := &types.ServiceStatusInfo{
		Status:             service.Status,
		DesiredInstances:   service.Scale,
		ObservedGeneration: observedGeneration,
	}

	// Count ready instances
	for _, instance := range instances {
		if instance.Status == types.InstanceStatusRunning {
			status.RunningInstances++
		}
	}

	return status, nil
}

func (sc *serviceController) UpdateServiceStatus(ctx context.Context, service *types.Service, status types.ServiceStatus) error {
	// Write ONLY the Status field, atomically, on the freshly-read service so a
	// concurrent scale write (the scaling controller) is never clobbered — the
	// caller's `service` is a snapshot that may be stale by now (RFC #129).
	var fresh types.Service
	if err := sc.store.UpdateFunc(ctx, types.ResourceTypeService, service.Namespace, service.Name, &fresh, func() error {
		fresh.Status = status
		return nil
	}); err != nil {
		return fmt.Errorf("failed to update service status: %w", err)
	}
	service.Status = status
	return nil
}

func (sc *serviceController) GetServiceLogs(ctx context.Context, namespace, name string, opts types.LogOptions) (io.ReadCloser, error) {
	// Get service from store
	var service types.Service
	if err := sc.store.Get(ctx, types.ResourceTypeService, namespace, name, &service); err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}

	// List instances for this service
	instances, err := sc.listInstancesForService(ctx, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	if len(instances) == 0 {
		return nil, fmt.Errorf("no instances found for service %s in namespace %s", name, namespace)
	}

	// First pass: collect logs from live (Running) instances. Wrap
	// each in a peekingReader so we can tell later whether the
	// stream is actually going to yield anything — without this we
	// can't distinguish "container produced 5MB of output" from
	// "container is silent" and the tombstone fallback never fires
	// for the silent case. (Live-observed on prod/gateway:
	// docker logs returned 0 bytes for the live container while a
	// previous tombstone had 14KB — `rune logs gateway` returned
	// nothing.)
	logInfos := make([]utils.InstanceLogInfo, 0, len(instances))
	peekers := make([]*peekingReader, 0)
	for _, instance := range instances {
		if instance.Status != types.InstanceStatusRunning {
			continue
		}
		if !opts.ShowLogs && !opts.ShowEvents && !opts.ShowStatus {
			sc.logger.Debug("Skipping running instance - no log types enabled",
				log.Str("instance_id", instance.ID))
			continue
		}
		logReader, err := sc.instanceController.GetInstanceLogs(ctx, instance, opts)
		if err != nil {
			sc.logger.Warn("Failed to get logs for running instance; will try tombstones",
				log.Str("service", name),
				log.Str("namespace", namespace),
				log.Str("instance", instance.ID),
				log.Err(err))
			continue
		}
		pr := newPeekingReader(logReader)
		peekers = append(peekers, pr)
		logInfos = append(logInfos, utils.InstanceLogInfo{
			InstanceID:   instance.ID,
			InstanceName: instance.Name,
			Reader:       pr,
		})
	}

	// Second pass — tombstone fallback. Two trigger conditions:
	//   (a) No live instances at all: collect the most-recent
	//       tombstone (already supported).
	//   (b) Live instances exist but all are SILENT (zero bytes
	//       observed via the peek). Skip in --follow mode because
	//       peeking would block until the live stream produces
	//       output — defeating follow semantics.
	needFallback := len(logInfos) == 0
	if !needFallback && !opts.Follow {
		allLiveEmpty := true
		for _, p := range peekers {
			if has, _ := p.HasData(); has {
				allLiveEmpty = false
				break
			}
		}
		needFallback = allLiveEmpty
	}
	if needFallback {
		if tomb := pickMostRecentTombstone(instances); tomb != nil {
			sc.logger.Info("Live instances silent or absent; serving most-recent tombstone snapshot",
				log.Str("service", name),
				log.Str("namespace", namespace),
				log.Str("tombstone", tomb.ID),
				log.Str("status", string(tomb.Status)),
				log.Int("live_count", len(logInfos)))
			logReader, err := sc.instanceController.GetInstanceLogs(ctx, tomb, opts)
			if err == nil {
				logInfos = append(logInfos, utils.InstanceLogInfo{
					InstanceID:   tomb.ID + " (previous)",
					InstanceName: tomb.Name + " (previous)",
					Reader:       logReader,
				})
			} else {
				sc.logger.Warn("Failed to read LastLogs from tombstone",
					log.Str("tombstone", tomb.ID),
					log.Err(err))
			}
		}
	}

	sc.logger.Debug("Collected log streams",
		log.Str("service", name),
		log.Int("streams", len(logInfos)))

	if len(logInfos) == 0 {
		return nil, fmt.Errorf("no logs available for service %s in namespace %s: no running instances and no tombstone snapshots", name, namespace)
	}

	// Always use MultiLogStreamer to ensure consistent metadata handling
	// regardless of whether we have one or multiple log readers
	return utils.NewMultiLogStreamer(logInfos, true), nil
}

// pickMostRecentTombstone returns the newest non-Running instance,
// prioritising Failed tombstones (postmortem-preserved containers)
// over Deleted ones, and preferring tombstones that carry a
// LastLogs snapshot. The "no-snapshot" tombstones are still
// candidates because GetInstanceLogs synthesises a "why-this-died"
// one-liner for terminal instances with no captured output — so a
// crashed-without-logs container still produces something useful at
// the service-level `rune logs`. Newness is determined by FailedAt
// when present, falling back to UpdatedAt.
func pickMostRecentTombstone(instances []*types.Instance) *types.Instance {
	// Prefer-with-logs is a two-tier preference: among Failed, take
	// the newest WITH logs; if none have logs, take the newest
	// without. Then fall back to Deleted with the same rule.
	var failedWithLogs, failedAny, deletedWithLogs, deletedAny *types.Instance
	for _, inst := range instances {
		switch inst.Status {
		case types.InstanceStatusFailed:
			if failedAny == nil || tombstoneTime(inst).After(tombstoneTime(failedAny)) {
				failedAny = inst
			}
			if len(inst.LastLogs) > 0 && (failedWithLogs == nil || tombstoneTime(inst).After(tombstoneTime(failedWithLogs))) {
				failedWithLogs = inst
			}
		case types.InstanceStatusDeleted:
			if deletedAny == nil || tombstoneTime(inst).After(tombstoneTime(deletedAny)) {
				deletedAny = inst
			}
			if len(inst.LastLogs) > 0 && (deletedWithLogs == nil || tombstoneTime(inst).After(tombstoneTime(deletedWithLogs))) {
				deletedWithLogs = inst
			}
		}
	}
	switch {
	case failedWithLogs != nil:
		return failedWithLogs
	case deletedWithLogs != nil:
		return deletedWithLogs
	case failedAny != nil:
		return failedAny
	}
	return deletedAny
}

// tombstoneTime returns the best wall-clock anchor for ordering
// tombstones — FailedAt when set (the moment the postmortem was
// taken), else UpdatedAt (the last lifecycle write).
func tombstoneTime(inst *types.Instance) time.Time {
	if inst.FailedAt != nil {
		return *inst.FailedAt
	}
	return inst.UpdatedAt
}

// peekingReader wraps an io.ReadCloser so we can answer "is this
// stream actually going to produce anything?" without consuming the
// data. The first byte is buffered on Peek/HasData and re-emitted
// on the next Read, so the wrapper is transparent to downstream
// consumers (MultiLogStreamer, the CLI client). Used by
// GetServiceLogs to detect silent live containers and fall back to
// the previous tombstone's LastLogs.
type peekingReader struct {
	rc       io.ReadCloser
	peek     []byte // buffered first byte; len() > 0 means "stream had data"
	peeked   bool   // first read has been attempted
	peekDone bool   // returned EOF/error during peek (no more data)
}

func newPeekingReader(rc io.ReadCloser) *peekingReader {
	return &peekingReader{rc: rc}
}

// HasData returns true if the underlying stream produced at least
// one byte. Triggers the lazy first read on first call. Safe to call
// multiple times; cached. On a read error the byte (if any) is still
// surfaced — we don't want to treat a partial first-byte-then-error
// as "no data" and silently mask the real failure.
func (p *peekingReader) HasData() (bool, error) {
	if p.peeked {
		return len(p.peek) > 0, nil
	}
	p.peeked = true
	buf := make([]byte, 1)
	n, err := p.rc.Read(buf)
	if n > 0 {
		p.peek = buf[:n]
	}
	if err == io.EOF {
		p.peekDone = true
		return n > 0, nil
	}
	return n > 0, err
}

func (p *peekingReader) Read(buf []byte) (int, error) {
	if len(p.peek) > 0 {
		n := copy(buf, p.peek)
		p.peek = p.peek[n:]
		return n, nil
	}
	if p.peekDone {
		return 0, io.EOF
	}
	return p.rc.Read(buf)
}

func (p *peekingReader) Close() error { return p.rc.Close() }

func (sc *serviceController) ExecInService(ctx context.Context, namespace, serviceName string, options types.ExecOptions) (types.ExecStream, error) {
	// Get service from store
	var service types.Service
	if err := sc.store.Get(ctx, types.ResourceTypeService, namespace, serviceName, &service); err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}

	// List instances for this service
	instances, err := sc.listInstancesForService(ctx, namespace, serviceName)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	if len(instances) == 0 {
		return nil, fmt.Errorf("no instances found for service %s in namespace %s", serviceName, namespace)
	}

	// Find a running instance
	var runningInstance *types.Instance
	for _, instance := range instances {
		if instance.Status == types.InstanceStatusRunning {
			runningInstance = instance
			break
		}
	}

	if runningInstance == nil {
		return nil, fmt.Errorf("no running instances found for service %s in namespace %s", serviceName, namespace)
	}

	// Execute command in the selected instance
	return sc.instanceController.Exec(ctx, runningInstance, options)
}

// DialInService picks the first Running instance for the service and
// opens a TCP connection to the given port on it. Mirrors
// ExecInService's selection semantics. Returns the chosen instance so
// the caller can surface it (e.g. in a PortForwardReady frame).
func (sc *serviceController) DialInService(ctx context.Context, namespace, serviceName string, port uint32) (net.Conn, *types.Instance, error) {
	var service types.Service
	if err := sc.store.Get(ctx, types.ResourceTypeService, namespace, serviceName, &service); err != nil {
		return nil, nil, fmt.Errorf("failed to get service: %w", err)
	}

	instances, err := sc.listInstancesForService(ctx, namespace, serviceName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list instances: %w", err)
	}
	if len(instances) == 0 {
		return nil, nil, fmt.Errorf("no instances found for service %s in namespace %s", serviceName, namespace)
	}

	var runningInstance *types.Instance
	for _, instance := range instances {
		if instance.Status == types.InstanceStatusRunning {
			runningInstance = instance
			break
		}
	}
	if runningInstance == nil {
		return nil, nil, fmt.Errorf("no running instances found for service %s in namespace %s", serviceName, namespace)
	}

	conn, err := sc.instanceController.Dial(ctx, runningInstance, port)
	if err != nil {
		return nil, nil, err
	}
	return conn, runningInstance, nil
}

func (sc *serviceController) RestartService(ctx context.Context, namespace, serviceName string) error {
	// Get service from store
	var service types.Service
	if err := sc.store.Get(ctx, types.ResourceTypeService, namespace, serviceName, &service); err != nil {
		return fmt.Errorf("failed to get service: %w", err)
	}

	// List instances for this service
	instances, err := sc.listInstancesForService(ctx, namespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	sc.logger.Info("Restarting service",
		log.Str("name", serviceName),
		log.Str("namespace", namespace),
		log.Int("instance_count", len(instances)))

	// Restart all instances
	for _, instance := range instances {
		if err := sc.instanceController.RestartInstance(ctx, instance, InstanceRestartReasonManual); err != nil {
			sc.logger.Error("Failed to restart instance",
				log.Str("instance", instance.ID),
				log.Str("service", serviceName),
				log.Err(err))
			// Continue with other instances
		}
	}

	return nil
}

func (sc *serviceController) StopService(ctx context.Context, namespace, serviceName string) error {
	// Get service from store
	var service types.Service
	if err := sc.store.Get(ctx, types.ResourceTypeService, namespace, serviceName, &service); err != nil {
		return fmt.Errorf("failed to get service: %w", err)
	}

	// List instances for this service
	instances, err := sc.listInstancesForService(ctx, namespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	sc.logger.Info("Stopping service",
		log.Str("name", serviceName),
		log.Str("namespace", namespace),
		log.Int("instance_count", len(instances)))

	// Stop all instances
	for _, instance := range instances {
		if err := sc.instanceController.StopInstance(ctx, instance); err != nil {
			sc.logger.Error("Failed to stop instance",
				log.Str("instance", instance.ID),
				log.Str("service", serviceName),
				log.Err(err))
			// Continue with other instances
		}
	}

	// Note: We don't update the service status to "stopped" as there's no such status
	// The service remains in its current status, but instances are stopped
	sc.logger.Info("Service instances stopped",
		log.Str("service", serviceName),
		log.Str("namespace", namespace))

	return nil
}

func (sc *serviceController) DeleteService(ctx context.Context, request *types.DeletionRequest) (*types.DeletionResponse, error) {
	// Get service for deletion
	service, err := sc.getServiceForDeletion(ctx, request)
	if err != nil {
		return nil, err
	}

	// Determine finalizer types based on service configuration
	finalizerTypes := sc.determineFinalizerTypes(service)

	// Handle dry run deletion
	if request.DryRun {
		return sc.handleDryRunDeletion(ctx, service, request, finalizerTypes)
	}

	// Handle real deletion
	return sc.handleRealDeletion(ctx, service, request, finalizerTypes)
}

func (sc *serviceController) GetDeletionStatus(ctx context.Context, namespace, name string) (*types.DeletionOperation, error) {
	// Get deletion operation from store
	var deletionOperation types.DeletionOperation
	if err := sc.store.Get(ctx, types.ResourceTypeDeletionOperation, namespace, name, &deletionOperation); err != nil {
		return nil, fmt.Errorf("failed to get deletion operation: %w", err)
	}

	return &deletionOperation, nil
}

func (sc *serviceController) listInstancesForService(ctx context.Context, namespace, serviceName string) ([]*types.Instance, error) {
	// Get all instances
	var instances []types.Instance
	err := sc.store.List(ctx, types.ResourceTypeInstance, namespace, &instances)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	// Filter instances for this service
	filteredInstances := make([]*types.Instance, 0, len(instances))
	for _, instance := range instances {
		if instance.ServiceName == serviceName {
			filteredInstances = append(filteredInstances, &instance)
		}
	}

	return filteredInstances, nil
}

func (sc *serviceController) WatchServices(ctx context.Context) (<-chan store.WatchEvent, error) {
	// Start watching services
	watchCh, err := sc.store.Watch(ctx, types.ResourceTypeService, "")
	if err != nil {
		return nil, fmt.Errorf("failed to watch services: %w", err)
	}
	return watchCh, nil
}

func (sc *serviceController) processServiceEvent(ctx context.Context, event store.WatchEvent) error {
	sc.logger.Debug("Processing service event",
		log.Str("name", event.Name),
		log.Str("namespace", event.Namespace),
		log.Str("type", string(event.Type)))

	// Get the service from the store
	var service types.Service
	if err := sc.store.Get(ctx, event.ResourceType, event.Namespace, event.Name, &service); err != nil {
		// If the resource doesn't exist and this is a delete event, that's ok
		if event.Type == store.WatchEventDeleted {
			return nil
		}
		return fmt.Errorf("failed to get service %s/%s: %w", event.Namespace, event.Name, err)
	}

	// Process based on event type
	switch event.Type {
	case store.WatchEventCreated:
		return sc.handleServiceCreated(ctx, &service)
	case store.WatchEventUpdated:
		return sc.handleServiceUpdated(ctx, &service)
	case store.WatchEventDeleted:
		sc.handleServiceDeleted(ctx, &service)
		return nil // Explicitly set to nil since handleServiceDeleted doesn't return an error
	default:
		return fmt.Errorf("unknown event type: %s", event.Type)
	}
}

// Helper methods for service lifecycle management

// recordObservedGeneration records the observed generation for a service
func (sc *serviceController) recordObservedGeneration(namespace, name string, generation int64) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	sc.serviceObservedLock.Lock()
	defer sc.serviceObservedLock.Unlock()
	sc.serviceObservedGenerations[key] = generation
}

// getObservedGeneration gets the observed generation for a service
func (sc *serviceController) getObservedGeneration(namespace, name string) (int64, bool) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	sc.serviceObservedLock.RLock()
	defer sc.serviceObservedLock.RUnlock()
	gen, exists := sc.serviceObservedGenerations[key]
	return gen, exists
}

// recordObservedScale records the last reconciled desired scale for a service.
func (sc *serviceController) recordObservedScale(namespace, name string, scale int) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	sc.serviceObservedLock.Lock()
	defer sc.serviceObservedLock.Unlock()
	sc.serviceObservedScales[key] = scale
}

// getObservedScale gets the last reconciled desired scale for a service.
func (sc *serviceController) getObservedScale(namespace, name string) (int, bool) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	sc.serviceObservedLock.RLock()
	defer sc.serviceObservedLock.RUnlock()
	scale, exists := sc.serviceObservedScales[key]
	return scale, exists
}

// isStatusOnlyChange checks if only the status was updated (not the spec)
func (sc *serviceController) isStatusOnlyChange(service *types.Service) bool {
	// Check if we've observed this generation before
	lastObserved, exists := sc.getObservedGeneration(service.Namespace, service.Name)
	if !exists {
		return false // We haven't seen this service before
	}

	// A scale change is a desired-state change that must reconcile, even though
	// the ScalingController applies it without bumping Generation. If the scale
	// differs from the last reconciled value (or we've never recorded one),
	// this is not a status-only change. Without this, scale-ups — including
	// `rune restart`'s scale-back-up — sit uncreated until the periodic 30s
	// reconcile tick, which reads as the service "hanging" at the lower scale.
	lastScale, scaleObserved := sc.getObservedScale(service.Namespace, service.Name)
	if !scaleObserved || lastScale != service.Scale {
		return false
	}

	sc.logger.Debug("Checking if status-only change",
		log.Str("namespace", service.Namespace),
		log.Str("name", service.Name),
		log.Int64("last_observed", lastObserved),
		log.Int64("current_generation", service.Metadata.Generation))

	// If the generation hasn't changed, it's a status-only update
	return lastObserved == service.Metadata.Generation
}

// StartWatching starts watching for service events
func (sc *serviceController) StartWatching(ctx context.Context) error {
	sc.logger.Info("Starting service watch")

	// Start watching services
	watchCh, err := sc.store.Watch(ctx, types.ResourceTypeService, "")
	if err != nil {
		return fmt.Errorf("failed to watch services: %w", err)
	}
	sc.watchCh = watchCh

	// Start a goroutine to process service events
	sc.wg.Add(1)
	go func() {
		defer sc.wg.Done()
		sc.watchServices()
	}()

	return nil
}

// StopWatching stops watching for service events
func (sc *serviceController) StopWatching() error {
	sc.logger.Info("Stopping service watch")

	// The watch will be stopped when the context is cancelled
	// This method is mainly for logging and future extensibility
	return nil
}

// watchServices watches service events and processes them
func (sc *serviceController) watchServices() {
	for {
		select {
		case <-sc.ctx.Done():
			return
		case event, ok := <-sc.watchCh:
			if !ok {
				sc.logger.Error("Service watch channel closed, restarting watch")
				// Try to restart the watch
				watchCh, err := sc.store.Watch(sc.ctx, types.ResourceTypeService, "")
				if err != nil {
					sc.logger.Error("Failed to restart service watch", log.Err(err))
					// Check if the store is closed
					if err.Error() == "store is closed, cannot create new watch" {
						sc.logger.Info("Store is closed, stopping service watch")
						return
					}
					time.Sleep(5 * time.Second) // Backoff before retry
					continue
				}
				sc.watchCh = watchCh
				continue
			}

			// Process the event directly (simplified version without worker queue).
			// Reconcile-loop suppression is handled downstream by isStatusOnlyChange
			// (generation + observed-scale tracking); the service controller does not
			// filter its own writes here.
			if err := sc.processServiceEvent(sc.ctx, event); err != nil {
				sc.logger.Error("Failed to process service event",
					log.Str("name", event.Name),
					log.Str("namespace", event.Namespace),
					log.Str("type", string(event.Type)),
					log.Err(err))
			}
		}
	}
}

// Deletion helper methods

// getServiceForDeletion retrieves and validates a service for deletion
func (sc *serviceController) getServiceForDeletion(ctx context.Context, request *types.DeletionRequest) (*types.Service, error) {
	var service types.Service
	if err := sc.store.Get(ctx, types.ResourceTypeService, request.Namespace, request.Name, &service); err != nil {
		if request.IgnoreNotFound {
			return nil, fmt.Errorf("service not found: %s/%s", request.Namespace, request.Name)
		}
		return nil, fmt.Errorf("service not found: %w", err)
	}
	return &service, nil
}

// determineFinalizerTypes determines which finalizers are needed for this service
func (sc *serviceController) determineFinalizerTypes(service *types.Service) []types.FinalizerType {
	finalizers := []types.FinalizerType{
		types.FinalizerTypeInstanceCleanup,
	}
	// Volume cleanup is only relevant when the service auto-provisioned
	// per-replica volumes via a claimTemplate. Bare Claim references are
	// to operator-owned volumes and are intentionally not reclaimed on
	// service deletion.
	if serviceHasClaimTemplate(service) {
		finalizers = append(finalizers, types.FinalizerTypeVolumeCleanup)
	}
	finalizers = append(finalizers, types.FinalizerTypeServiceDeregister)
	return finalizers
}

// serviceHasClaimTemplate reports whether the service declares any
// volume mount that uses a claimTemplate (per-replica auto-provisioning).
func serviceHasClaimTemplate(service *types.Service) bool {
	if service == nil {
		return false
	}
	for _, v := range service.Volumes {
		if v.ClaimTemplate != nil {
			return true
		}
	}
	return false
}

// handleDryRunDeletion handles dry run deletion requests
func (sc *serviceController) handleDryRunDeletion(ctx context.Context, service *types.Service, request *types.DeletionRequest, finalizerTypes []types.FinalizerType) (*types.DeletionResponse, error) {
	// Create finalizers with pending status for dry run
	finalizers := sc.createFinalizersFromTypes(finalizerTypes)

	// Use shared validation logic
	errors, warnings := sc.validateDeletionRequest(ctx, service, request)

	// If there are errors, return failure response
	if len(errors) > 0 {
		return &types.DeletionResponse{
			DeletionID: "dry-run",
			Status:     "failed",
			Errors:     errors,
			Finalizers: finalizers,
		}, nil
	}

	return &types.DeletionResponse{
		DeletionID: "dry-run",
		Status:     "dry_run",
		Finalizers: finalizers,
		Warnings:   warnings,
	}, nil
}

// handleRealDeletion handles real deletion requests
func (sc *serviceController) handleRealDeletion(ctx context.Context, service *types.Service, request *types.DeletionRequest, finalizerTypes []types.FinalizerType) (*types.DeletionResponse, error) {
	// Use shared validation logic
	errors, _ := sc.validateDeletionRequest(ctx, service, request)

	// If there are validation errors, return failure response
	if len(errors) > 0 {
		return &types.DeletionResponse{
			Status: "failed",
			Errors: errors,
		}, fmt.Errorf("deletion validation failed: %s", strings.Join(errors, "; "))
	}

	// Create deletion task ID
	taskID := fmt.Sprintf("delete-%s-%s-%d", service.Namespace, service.Name, time.Now().Unix())

	// Create and store the deletion operation
	deletionOperation := &types.DeletionOperation{
		ID:               taskID,
		Namespace:        service.Namespace,
		ServiceName:      service.Name,
		TotalInstances:   service.Scale,
		DeletedInstances: 0,
		FailedInstances:  0,
		StartTime:        time.Now(),
		Status:           types.DeletionOperationStatusInitializing,
		DryRun:           false,
		Finalizers:       sc.createFinalizersFromTypes(finalizerTypes),
	}

	// Store the deletion operation
	if err := sc.store.Create(ctx, types.ResourceTypeDeletionOperation, service.Namespace, taskID, deletionOperation); err != nil {
		sc.logger.Error("Failed to store deletion operation", log.Err(err))
		// Continue anyway - the operation can still proceed
	}

	// Submit to worker pool
	if err := sc.submitDeletionTask(ctx, taskID, service, request, finalizerTypes, deletionOperation); err != nil {
		return &types.DeletionResponse{
			Status: "failed",
			Errors: []string{fmt.Sprintf("failed to submit deletion task: %v", err)},
		}, err
	}

	return &types.DeletionResponse{
		DeletionID: taskID,
		Status:     "in_progress",
		Finalizers: deletionOperation.Finalizers,
	}, nil
}

// submitDeletionTask submits a deletion task to the worker pool
func (sc *serviceController) submitDeletionTask(ctx context.Context, taskID string, service *types.Service, request *types.DeletionRequest, finalizerTypes []types.FinalizerType, deletionOperation *types.DeletionOperation) error {
	// Create deletion task
	deletionTask := tasks.NewDeletionTask(
		taskID,
		service,
		request,
		finalizerTypes,
		sc.finalizerExecutor,
		sc.store,
		sc.logger,
	)
	// Submit to worker pool
	if err := sc.deletionWorkerPool.Submit(ctx, deletionTask); err != nil {
		// Update deletion operation status to failed
		deletionOperation.Status = types.DeletionOperationStatusFailed
		deletionOperation.FailureReason = fmt.Sprintf("failed to submit deletion task: %v", err)
		if updateErr := sc.store.Update(ctx, types.ResourceTypeDeletionOperation, service.Namespace, deletionTask.GetID(), deletionOperation); updateErr != nil {
			sc.logger.Error("Failed to update deletion operation status", log.Err(updateErr))
		}

		return err
	}

	sc.logger.Info("Submitted deletion task",
		log.Str("task_id", taskID),
		log.Str("service", deletionOperation.ServiceName),
		log.Str("namespace", deletionOperation.Namespace))
	return nil
}

// recoverInProgressDeletions marks stale in-progress deletion operations as failed so
// they do not block new delete requests after a runed crash or restart.
//
// This runs from serviceController.Start, which the server invokes (via
// orchestrator.Start) before the gRPC server is constructed — so no delete
// request can be in flight here. Any operation still in a non-terminal status
// is therefore orphaned by a dead process and safe to fail unconditionally.
//
// Recovery only unblocks retries: it does not resume the interrupted teardown.
// The service and any instances/finalizers are left in their partial state and
// the operation's progress counters are not corrected — the caller must
// re-issue the delete to finish the job.
func (sc *serviceController) recoverInProgressDeletions(ctx context.Context) error {
	sc.logger.Info("Checking for in-progress deletion operations to recover")

	var operations []types.DeletionOperation
	if err := sc.store.ListAll(ctx, types.ResourceTypeDeletionOperation, &operations); err != nil {
		return fmt.Errorf("failed to list deletion operations: %w", err)
	}

	var recovered int
	for _, op := range operations {
		switch op.Status {
		case types.DeletionOperationStatusInitializing,
			types.DeletionOperationStatusDeletingInstances,
			types.DeletionOperationStatusRunningFinalizers:
			// fall through
		default:
			continue
		}

		age := time.Since(op.StartTime)
		reason := "interrupted by server restart"
		if age > time.Hour {
			reason = "timed out during server recovery"
		}

		opCopy := op
		opCopy.Status = types.DeletionOperationStatusFailed
		opCopy.FailureReason = reason
		now := time.Now()
		opCopy.EndTime = &now

		if err := sc.store.Update(ctx, types.ResourceTypeDeletionOperation, opCopy.Namespace, opCopy.ID, &opCopy); err != nil {
			sc.logger.Error("Failed to mark stale deletion operation as failed",
				log.Str("operation_id", op.ID),
				log.Str("service", op.ServiceName),
				log.Str("namespace", op.Namespace),
				log.Err(err))
			continue
		}

		sc.logger.Warn("Marked stale deletion operation as failed",
			log.Str("operation_id", op.ID),
			log.Str("service", op.ServiceName),
			log.Str("namespace", op.Namespace),
			log.Str("previous_status", string(op.Status)),
			log.Duration("age", age))
		recovered++
	}

	sc.logger.Info("Deletion operation recovery complete", log.Int("recovered", recovered))
	return nil
}

// checkExistingDeletion checks if there's already a deletion operation in progress
func (sc *serviceController) checkExistingDeletion(ctx context.Context, service *types.Service) error {
	// List existing deletion operations for this service
	var operations []types.DeletionOperation
	err := sc.store.List(ctx, types.ResourceTypeDeletionOperation, service.Namespace, &operations)
	if err != nil {
		return fmt.Errorf("failed to list deletion operations: %w", err)
	}

	// Check for in-progress operations
	for _, op := range operations {
		if op.ServiceName == service.Name &&
			(op.Status == types.DeletionOperationStatusInitializing ||
				op.Status == types.DeletionOperationStatusDeletingInstances ||
				op.Status == types.DeletionOperationStatusRunningFinalizers) {
			return fmt.Errorf("deletion operation already in progress for service %s/%s (ID: %s)",
				service.Namespace, service.Name, op.ID)
		}
	}

	return nil
}

// checkServiceState validates the service state for deletion
func (sc *serviceController) checkServiceState(ctx context.Context, service *types.Service) error {
	// Check if service is in a deletable state
	if service.Status == types.ServiceStatusDeleted {
		return fmt.Errorf("service %s/%s is already deleted", service.Namespace, service.Name)
	}

	// Add any other service state validations here
	return nil
}

// checkSystemReadiness checks if the system is ready for deletion
func (sc *serviceController) checkSystemReadiness(ctx context.Context) error {
	// For now, assume the system is ready
	// TODO: Add actual system readiness checks when needed
	return nil
}

// checkWorkerPoolCapacity checks if the worker pool has capacity
func (sc *serviceController) checkWorkerPoolCapacity() error {
	// This is a placeholder - we need to access the deletionWorkerPool
	// For now, assume capacity is available
	return nil
}

// validateDeletionRequest validates a deletion request
func (sc *serviceController) validateDeletionRequest(ctx context.Context, service *types.Service, request *types.DeletionRequest) ([]string, []string) {
	var errors []string
	var warnings []string

	// Check for existing deletion
	if err := sc.checkExistingDeletion(ctx, service); err != nil {
		errors = append(errors, err.Error())
	}

	// Check service state
	if err := sc.checkServiceState(ctx, service); err != nil {
		errors = append(errors, err.Error())
	}

	// Check system readiness
	if err := sc.checkSystemReadiness(ctx); err != nil {
		errors = append(errors, err.Error())
	}

	// Check worker pool capacity
	if err := sc.checkWorkerPoolCapacity(); err != nil {
		errors = append(errors, err.Error())
	}

	// Add any other validations here
	// For example, check if service has dependencies that would prevent deletion

	return errors, warnings
}

// createFinalizersFromTypes creates finalizer objects from finalizer types
func (sc *serviceController) createFinalizersFromTypes(finalizerTypes []types.FinalizerType) []types.Finalizer {
	finalizers := make([]types.Finalizer, 0, len(finalizerTypes))
	now := time.Now()
	for _, ft := range finalizerTypes {
		finalizer := types.Finalizer{
			ID:        fmt.Sprintf("%s-%d", string(ft), now.Unix()),
			Type:      ft,
			Status:    types.FinalizerStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		}
		finalizers = append(finalizers, finalizer)
	}
	return finalizers
}
