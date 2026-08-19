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
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
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
	RestartService(ctx context.Context, namespace, serviceName string) (templateGeneration int64, scale int, err error)
	StopService(ctx context.Context, namespace, serviceName string) error
	DeleteService(ctx context.Context, request *types.DeletionRequest) (*types.DeletionResponse, error)
	GetDeletionStatus(ctx context.Context, namespace, name string) (*types.DeletionOperation, error)
	listInstancesForService(ctx context.Context, namespace, serviceName string) ([]*types.Instance, error)
}

// serviceController implements the ServiceController interface
type serviceController struct {
	store              store.Store
	instanceController InstanceController
	healthController   HealthController
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

	// Watch channel for instances (event-driven status roll-up, Phase 3c)
	instanceWatchCh <-chan store.WatchEvent
}

// NewServiceController creates a new service controller
func NewServiceController(
	store store.Store,
	instanceController InstanceController,
	healthController HealthController,
	logger log.Logger,
) (ServiceController, error) {
	// Create the reconciler once. It owns the deletion cascade too: a
	// tombstoned service (Metadata.DeletionTimestamp set) is torn down by
	// reconcileDeletion on the single-writer workqueue — no separate deletion
	// worker pool or async finalizer executor (RFC #129 Phase 4).
	reconciler := newReconciler(
		store,
		instanceController,
		healthController,
		logger.WithComponent("service-reconciler"),
	)

	return &serviceController{
		store:              store,
		instanceController: instanceController,
		healthController:   healthController,
		reconciler:         reconciler,
		logger:             logger.WithComponent("service-controller"),
	}, nil
}

// Start starts the service controller
func (sc *serviceController) Start(ctx context.Context) error {
	sc.logger.Info("Starting service controller")

	// Create a context with cancel for all background operations
	sc.ctx, sc.cancel = context.WithCancel(ctx)

	// No deletion-recovery step: a tombstoned service is just another service
	// the reconciler re-drives on boot (enqueueAllServices lists it because
	// its record still exists) and on the 30s resync, so an interrupted
	// teardown resumes with no separate recovery path (RFC #129 Phase 4).

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

// enqueueServiceEvent translates a service watch event into a reconcile
// enqueue on the per-key workqueue (RFC #129 Phase 3). Status-only echoes —
// updates whose ObservedGeneration already equals Generation, i.e. the
// reconciler's own status writes bouncing back through the watch — are dropped
// here as an optimization; correctness never depends on this filter because
// the reconcile is idempotent and the periodic resync is level-triggered.
//
// Created and Deleted events always enqueue: syncService re-reads fresh state
// and treats a missing service as settled, so a deleted service's key simply
// drains. No handler logic runs on the watch goroutine anymore — the queue
// workers own all reconciliation, which is what makes reconciles of one
// service impossible to interleave.
func (sc *serviceController) enqueueServiceEvent(event store.WatchEvent) {
	if event.Type == store.WatchEventUpdated {
		// The watch event carries the written resource; tolerate both value
		// and pointer forms. If it isn't inspectable, enqueue — never guess
		// in the direction of dropping work.
		var svc *types.Service
		switch v := event.Resource.(type) {
		case *types.Service:
			svc = v
		case types.Service:
			svc = &v
		}
		if svc != nil && sc.isStatusOnlyChange(svc) {
			sc.logger.Debug("Skipping enqueue for status-only change",
				log.Str("name", event.Name),
				log.Str("namespace", event.Namespace))
			return
		}
	}

	sc.reconciler.Enqueue(event.Namespace, event.Name)
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

	var observedGeneration int64
	if service.Metadata != nil {
		observedGeneration = service.Metadata.ObservedGeneration
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

// RestartService restarts a service the first-class way (issue #140): a
// single atomic template restamp. Bumping Generation makes the reconciler
// fire (desired-state change), and stamping TemplateGeneration = Generation
// makes every existing instance template-stale, so the reconciler replaces
// them all at the current spec — the `kubectl rollout restart` pattern. The
// desired scale never dips through zero (the old implementation was two
// racing scale writes whose drain leg could be skipped entirely).
// Restarting a stopped service (scale 0) starts it at its last non-zero
// scale.
//
// Returns the stamped template generation and the scale the service will
// converge to: instances created for this restart record a generation >=
// the returned value, which is what clients wait on.
func (sc *serviceController) RestartService(ctx context.Context, namespace, serviceName string) (int64, int, error) {
	var fresh types.Service
	err := sc.store.UpdateFunc(ctx, types.ResourceTypeService, namespace, serviceName, &fresh, func() error {
		if fresh.Status == types.ServiceStatusDeleted {
			return fmt.Errorf("service %s/%s is deleted", namespace, serviceName)
		}
		if fresh.Metadata == nil {
			fresh.Metadata = &types.ServiceMetadata{}
		}
		fresh.Metadata.Generation++
		fresh.Metadata.TemplateGeneration = fresh.Metadata.Generation
		// Restarting a stopped service means "start it again".
		if fresh.Scale == 0 {
			restored := fresh.Metadata.LastNonZeroScale
			if restored < 1 {
				restored = 1
			}
			fresh.Scale = restored
		}
		fresh.Metadata.UpdatedAt = time.Now()
		return nil
	}, store.WithOrchestrator())
	if err != nil {
		return 0, 0, fmt.Errorf("failed to restart service: %w", err)
	}

	// Re-arm stuck-in-create records.
	//
	// A restamped template generation replaces normal instances, but does
	// nothing for one that never got a container: the compatibility gate
	// deliberately reports those as "compatible" (so automatic reconciles
	// don't spew UUID confetti), and the reconciler holds a Stalled one
	// outright as "operator action required". Restart IS that operator
	// action — the reconciler's own comment says a Stalled record is re-armed
	// by running restart — so without this, `rune restart` on a service whose
	// instance is Stalled silently did nothing while the CLI waited out its
	// full timeout printing "0/N replaced and ready".
	//
	// Clearing the backoff as well as the status matters: restart means "try
	// again NOW", not "try again when the schedule next allows". Resetting
	// CreateAttempts gives the slot a fresh budget before it can re-stall.
	rearmed := sc.rearmStuckInCreateInstances(ctx, namespace, serviceName)

	sc.logger.Info("Service restart stamped",
		log.Str("name", serviceName),
		log.Str("namespace", namespace),
		log.Int64("template_generation", fresh.Metadata.TemplateGeneration),
		log.Int("scale", fresh.Scale),
		log.Int("rearmed_instances", rearmed))

	return fresh.Metadata.TemplateGeneration, fresh.Scale, nil
}

// rearmStuckInCreateInstances resets instances that never got a container
// (Stalled, or Failed with a pending backoff) so the reconciler retries them
// immediately, and reports how many were re-armed. Best-effort: the restamp
// above is already durable, so a failure here is logged rather than surfaced.
func (sc *serviceController) rearmStuckInCreateInstances(ctx context.Context, namespace, serviceName string) int {
	instances, err := sc.listInstancesForService(ctx, namespace, serviceName)
	if err != nil {
		sc.logger.Warn("Could not list instances to re-arm on restart",
			log.Str("service", serviceName), log.Err(err))
		return 0
	}

	rearmed := 0
	for _, inst := range instances {
		if inst == nil || inst.ContainerEverCreatedAt != nil {
			continue // had a container: the generation restamp replaces it normally
		}
		if inst.Status != types.InstanceStatusStalled && inst.Status != types.InstanceStatusFailed {
			continue
		}
		var fresh types.Instance
		skipped := false
		err := sc.store.UpdateFunc(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &fresh, func() error {
			// Re-check on the fresh copy: it may have moved on since listing.
			if fresh.ContainerEverCreatedAt != nil ||
				(fresh.Status != types.InstanceStatusStalled && fresh.Status != types.InstanceStatusFailed) {
				skipped = true
				return store.ErrSkipUpdate
			}
			// Failed is the state the reconciler's retry-in-place branch acts
			// on; clearing NextCreateAttemptAt makes it act on the next tick.
			fresh.Status = types.InstanceStatusFailed
			fresh.CreateAttempts = 0
			fresh.NextCreateAttemptAt = nil
			fresh.UpdatedAt = time.Now()
			return nil
		}, store.WithOrchestrator())
		if err != nil {
			sc.logger.Warn("Failed to re-arm stuck-in-create instance on restart",
				log.Str("instance", inst.Name), log.Err(err))
			continue
		}
		if skipped {
			// ErrSkipUpdate surfaces as a nil error, so the write only
			// happened if the re-check let it through.
			continue
		}
		rearmed++
		sc.logger.Info("Re-armed stuck-in-create instance on restart",
			log.Str("service", serviceName),
			log.Str("namespace", namespace),
			log.Str("instance", inst.Name))
	}
	return rearmed
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

	// Withdraw the whole service from the dataplane first and take one
	// shared drain window (RUNE-042 §4): in-flight requests finish against
	// containers that are no longer receiving new connections, and the
	// per-instance StopInstance calls below skip their own drains.
	sc.instanceController.WithdrawServiceInstances(ctx, &service, instances)

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

// Helper methods for service lifecycle management

// isStatusOnlyChange reports whether a service update carries only status /
// observed fields (no desired-state change), in which case reconciliation can
// be skipped to avoid a self-triggered loop.
//
// Every desired-state change bumps Metadata.Generation — spec edits (via cast)
// and scale changes (via the scaling controller, RFC #129 Phase 2). The
// reconciler records the generation it last converged on in
// Metadata.ObservedGeneration. So when the two are equal, nothing the
// reconciler acts on has changed since it last ran, and this event is a
// status-only echo. This replaces the old in-memory
// serviceObservedGenerations/serviceObservedScales maps, which were wiped on
// every runed restart and depended on the fragile invariant "scale changes
// don't bump Generation".
func (sc *serviceController) isStatusOnlyChange(service *types.Service) bool {
	if service.Metadata == nil {
		// No generation bookkeeping yet — treat as needing reconciliation.
		return false
	}
	return service.Metadata.ObservedGeneration == service.Metadata.Generation
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

	// Watch instances too: an instance status change (health promotion,
	// failure, deletion) enqueues its owning service so status rolls up
	// event-driven instead of waiting for the 30s resync (RFC #129 Phase 3c).
	instanceWatchCh, err := sc.store.Watch(ctx, types.ResourceTypeInstance, "")
	if err != nil {
		return fmt.Errorf("failed to watch instances: %w", err)
	}
	sc.instanceWatchCh = instanceWatchCh

	sc.wg.Add(1)
	go func() {
		defer sc.wg.Done()
		sc.watchInstances()
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

			// Translate the event into a workqueue enqueue. All reconciliation
			// happens on the queue workers (single writer per service key);
			// this goroutine never blocks on reconcile work.
			sc.enqueueServiceEvent(event)
		}
	}
}

// watchInstances watches instance events and enqueues the owning service for
// reconciliation (event-driven status roll-up). Same restart-on-close shape as
// watchServices.
func (sc *serviceController) watchInstances() {
	for {
		select {
		case <-sc.ctx.Done():
			return
		case event, ok := <-sc.instanceWatchCh:
			if !ok {
				sc.logger.Error("Instance watch channel closed, restarting watch")
				watchCh, err := sc.store.Watch(sc.ctx, types.ResourceTypeInstance, "")
				if err != nil {
					sc.logger.Error("Failed to restart instance watch", log.Err(err))
					if err.Error() == "store is closed, cannot create new watch" {
						sc.logger.Info("Store is closed, stopping instance watch")
						return
					}
					time.Sleep(5 * time.Second) // Backoff before retry
					continue
				}
				sc.instanceWatchCh = watchCh
				continue
			}

			sc.enqueueInstanceEvent(event)
		}
	}
}

// enqueueInstanceEvent maps an instance watch event to its owning service and
// enqueues that service for reconciliation. This is what makes service status
// converge within milliseconds of an instance transition (e.g. the health
// controller's Starting→Running promotion) instead of waiting for the 30s
// resync tick.
//
// Reconciler-sourced instance writes are skipped: the only such write is
// UpdateInstance during an in-flight sync, whose run already ends with
// updateServiceStatus — re-enqueueing it would be a pure echo. Every other
// source (health controller, API, finalizers, empty) enqueues; the reconcile
// is idempotent, so over-triggering is safe and under-triggering is what the
// resync safety net exists for.
func (sc *serviceController) enqueueInstanceEvent(event store.WatchEvent) {
	if event.Source == store.EventSourceReconciler {
		return
	}

	namespace, serviceName, ok := sc.ownerFromInstanceEvent(event)
	if !ok {
		return
	}
	sc.reconciler.Enqueue(namespace, serviceName)
}

// ownerFromInstanceEvent resolves the owning service of an instance event.
// It prefers the resource carried on the event (tolerating pointer and value
// forms) and falls back to reading the instance from the store. A deleted
// instance that can no longer be read is dropped — the periodic resync covers
// any staleness that could leave behind.
func (sc *serviceController) ownerFromInstanceEvent(event store.WatchEvent) (namespace, serviceName string, ok bool) {
	var inst *types.Instance
	switch v := event.Resource.(type) {
	case *types.Instance:
		inst = v
	case types.Instance:
		inst = &v
	}

	if inst == nil {
		ctx := sc.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		var fetched types.Instance
		if err := sc.store.Get(ctx, types.ResourceTypeInstance, event.Namespace, event.Name, &fetched); err != nil {
			sc.logger.Debug("Dropping unmappable instance event",
				log.Str("namespace", event.Namespace),
				log.Str("instance", event.Name),
				log.Str("type", string(event.Type)))
			return "", "", false
		}
		inst = &fetched
	}

	if inst.ServiceName == "" {
		return "", "", false
	}

	namespace = inst.Namespace
	if namespace == "" {
		namespace = event.Namespace
	}
	return namespace, inst.ServiceName, true
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

// handleRealDeletion tombstones the service for foreground deletion (RFC #129
// Phase 4): it stamps Metadata.DeletionTimestamp + the finalizer list on the
// record (via CAS) and enqueues it. The single-writer reconciler then drives
// reconcileDeletion — instances → volumes → record removal — with the record
// never leaving the store until every finalizer clears. There is no async
// worker pool: the deletion is just reconciliation of a tombstoned service.
func (sc *serviceController) handleRealDeletion(ctx context.Context, service *types.Service, request *types.DeletionRequest, finalizerTypes []types.FinalizerType) (*types.DeletionResponse, error) {
	// Idempotent re-issue: an already-tombstoned service is a no-op success,
	// not an error — the teardown is already in flight (checkServiceState
	// would otherwise reject "already deleted").
	if service.Metadata != nil && service.Metadata.DeletionTimestamp != nil {
		return sc.inProgressDeletionResponse(ctx, service), nil
	}

	// Validate (dependents/Force enforced upstream in the API handler; this
	// covers service-state and existing-deletion checks).
	errs, _ := sc.validateDeletionRequest(ctx, service, request)
	if len(errs) > 0 {
		return &types.DeletionResponse{
			Status: "failed",
			Errors: errs,
		}, fmt.Errorf("deletion validation failed: %s", strings.Join(errs, "; "))
	}

	// The finalizers the reconciler will run, in order. ServiceDeregister is
	// NOT a finalizer — record removal is the terminal transition when the
	// list empties, which is what makes the record provably outlive cleanup.
	fins := deletionFinalizerList(service)

	// Stamp the tombstone atomically. A concurrent re-issue that already
	// stamped it loses the CAS race and is treated as idempotent success.
	taskID := fmt.Sprintf("delete-%s-%s-%d", service.Namespace, service.Name, time.Now().Unix())
	var fresh types.Service
	err := sc.store.UpdateFunc(ctx, types.ResourceTypeService, service.Namespace, service.Name, &fresh, func() error {
		if fresh.Metadata == nil {
			fresh.Metadata = &types.ServiceMetadata{}
		}
		if fresh.Metadata.DeletionTimestamp != nil {
			return store.ErrSkipUpdate // already tombstoned by a racing re-issue
		}
		now := time.Now()
		fresh.Metadata.DeletionTimestamp = &now
		fresh.Metadata.Finalizers = fins
		fresh.Status = types.ServiceStatusDeleted
		return nil
	}, store.WithOrchestrator())
	if err != nil {
		return &types.DeletionResponse{
			Status: "failed",
			Errors: []string{fmt.Sprintf("failed to tombstone service: %v", err)},
		}, err
	}

	// Compatibility shim: the DeletionOperation record the CLI/dashboard poll
	// via GetDeletionStatus. reconcileDeletion advances it as teardown
	// progresses. Best-effort — the tombstoned service is the source of truth.
	deletionOperation := &types.DeletionOperation{
		ID:               taskID,
		Namespace:        service.Namespace,
		ServiceName:      service.Name,
		TotalInstances:   service.Scale,
		DeletedInstances: 0,
		FailedInstances:  0,
		StartTime:        time.Now(),
		Status:           types.DeletionOperationStatusDeletingInstances,
		DryRun:           false,
		Finalizers:       sc.createFinalizersFromTypes(finalizerTypes),
	}
	if err := sc.store.Create(ctx, types.ResourceTypeDeletionOperation, service.Namespace, taskID, deletionOperation); err != nil {
		sc.logger.Error("Failed to store deletion operation shim", log.Err(err))
		// Non-fatal: teardown proceeds regardless.
	}

	// Drive the teardown promptly (the 30s resync would otherwise pick it up).
	sc.reconciler.Enqueue(service.Namespace, service.Name)

	sc.logger.Info("Service tombstoned for deletion",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Any("finalizers", fins))

	return &types.DeletionResponse{
		DeletionID: taskID,
		Status:     "in_progress",
		Finalizers: deletionOperation.Finalizers,
	}, nil
}

// deletionFinalizerList returns the ordered finalizers the reconciler runs when
// tearing a service down: instance-cleanup first, then volume-cleanup iff the
// service auto-provisioned per-replica claimTemplate volumes. ServiceDeregister
// is intentionally absent — the record is removed as the terminal step once
// this list empties.
func deletionFinalizerList(service *types.Service) []types.FinalizerType {
	fins := []types.FinalizerType{types.FinalizerTypeInstanceCleanup}
	if serviceHasClaimTemplate(service) {
		fins = append(fins, types.FinalizerTypeVolumeCleanup)
	}
	return fins
}

// inProgressDeletionResponse builds the idempotent-re-issue response for an
// already-tombstoned service, reusing the existing shim operation if present.
func (sc *serviceController) inProgressDeletionResponse(ctx context.Context, service *types.Service) *types.DeletionResponse {
	resp := &types.DeletionResponse{
		DeletionID: fmt.Sprintf("delete-%s-%s", service.Namespace, service.Name),
		Status:     "in_progress",
	}
	var ops []types.DeletionOperation
	if err := sc.store.List(ctx, types.ResourceTypeDeletionOperation, service.Namespace, &ops); err == nil {
		for i := range ops {
			if ops[i].ServiceName == service.Name {
				resp.DeletionID = ops[i].ID
				resp.Finalizers = ops[i].Finalizers
				break
			}
		}
	}
	return resp
}

// checkExistingDeletion reports an error if the service is already being torn
// down. The source of truth is the tombstone on the record itself
// (Metadata.DeletionTimestamp), not a separate operation record. In practice
// handleRealDeletion short-circuits a tombstoned re-issue to success before
// validation ever runs, so this is a belt-and-suspenders guard for any other
// caller of validateDeletionRequest.
func (sc *serviceController) checkExistingDeletion(ctx context.Context, service *types.Service) error {
	if service.Metadata != nil && service.Metadata.DeletionTimestamp != nil {
		return fmt.Errorf("service %s/%s is already being deleted", service.Namespace, service.Name)
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
