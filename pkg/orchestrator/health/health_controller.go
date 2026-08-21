package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
	"github.com/runestack/rune/pkg/orchestrator/probes"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// Controller monitors instance health
type Controller interface {
	// Start the health controller
	Start(ctx context.Context) error

	// Stop the health controller
	Stop() error

	// AddInstance adds an instance to be monitored
	AddInstance(service *types.Service, instance *types.Instance) error

	// RemoveInstance removes an instance from monitoring
	RemoveInstance(instanceID string) error

	// GetHealthStatus gets the current health status of an instance
	GetHealthStatus(ctx context.Context, instanceID string) (*types.InstanceHealthStatus, error)
}

// healthController implements the Controller interface
type healthController struct {
	logger log.Logger

	// Monitored instances map
	instances map[string]*instanceHealth

	// Mutex for instances map
	mu sync.RWMutex

	// Context for background operations
	ctx    context.Context
	cancel context.CancelFunc

	// Mutex for context access
	ctxMu sync.RWMutex

	// HTTP client for health checks
	client *http.Client

	// Wait group for checker goroutines
	wg sync.WaitGroup

	// Store to retrieve service definitions
	store store.Store

	// Runners for executing commands
	runnerManager manager.IRunnerManager

	// Instance controller for restarting instances
	instanceController healthInstanceOps
}

// Ensure healthController implements RunnerProvider
var _ runner.RunnerProvider = (*healthController)(nil)

// GetInstanceRunner implements the RunnerProvider interface
func (c *healthController) GetInstanceRunner(instance *types.Instance) (runner.Runner, error) {
	return c.runnerManager.GetInstanceRunner(instance)
}

// instanceHealth tracks health check state for an instance
type instanceHealth struct {
	instance            *types.Instance
	service             *types.Service
	livenessResults     []types.HealthCheckResult
	readinessResults    []types.HealthCheckResult
	livenessStatus      bool
	readinessStatus     bool
	promoted            bool // true once promoteToRunningOnReady has succeeded
	monitoring          bool // true while monitorInstance goroutine is running
	lastCheck           time.Time
	consecutiveFailures int
	healthRestartCount  int // Separate count for health check restarts (backoff calculation)
	lastRestartTime     time.Time
}

// NewController creates a new health controller
// Republisher lets a caller that changed instance reachability refresh
// the dataplane endpoint set from current store state (RUNE-311
// Phase 3). Nil-safe when no endpoint publisher is wired; safe to call
// repeatedly.
type Republisher interface {
	RepublishServiceByInstance(ctx context.Context, instance *types.Instance)
}

// healthInstanceOps is the slice of the instance controller the health
// controller drives: restart on probe failure, republish on the
// Starting→Running promotion. The consumer owns the interface;
// *InstanceController satisfies it.
type healthInstanceOps interface {
	Republisher
	RestartInstance(ctx context.Context, instance *types.Instance, reason instancectl.RestartReason) error
}

func NewController(logger log.Logger, store store.Store, runnerManager manager.IRunnerManager, instanceController healthInstanceOps) Controller {
	return &healthController{
		logger:             logger.WithComponent("health-controller"),
		instances:          make(map[string]*instanceHealth),
		instanceController: instanceController,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		store:         store,
		runnerManager: runnerManager,
	}
}

// Start the health controller
func (c *healthController) Start(ctx context.Context) error {
	c.logger.Info("Starting health controller")

	// Create a context with cancel for all background operations
	c.ctxMu.Lock()
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.ctxMu.Unlock()

	return nil
}

// Stop the health controller
func (c *healthController) Stop() error {
	c.logger.Info("Stopping health controller")

	// Cancel context to stop all operations
	c.ctxMu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.ctx = nil // Set context to nil after cancellation
	}
	c.ctxMu.Unlock()

	// Wait for monitor goroutines; cap wait so Ctrl+C does not hang on
	// a stuck docker exec probe.
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		c.logger.Warn("Health controller stop timed out; probe goroutines may still be exiting")
	}

	return nil
}

// AddInstance adds an instance to be monitored
func (c *healthController) AddInstance(service *types.Service, instance *types.Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Info("Adding instance to health monitoring",
		log.Str("instance", instance.ID))

	// Check if instance is already being monitored
	if ih, exists := c.instances[instance.ID]; exists {
		// Refresh the service spec (probe type/command can change on
		// cast). Restart the monitor if a prior goroutine exited early
		// (e.g. health controller context was not ready yet).
		ih.service = service
		ih.instance = instance
		if ih.monitoring {
			c.logger.Debug("Instance already being monitored (refreshed service spec)",
				log.Str("instance", instance.ID))
			return nil
		}
		c.logger.Info("Restarting health monitor for instance (previous monitor exited)",
			log.Str("instance", instance.ID))
		c.startMonitorLocked(ih, instance.ID)
		return nil
	}

	// Seed CrashLoopBackoff state from the slot's PERSISTED restart
	// history. Every health restart replaces the instance with a new
	// UUID, so keying backoff purely on in-memory per-UUID state reset
	// it to zero each cycle — a chronically failing probe restarted the
	// slot every ~40s forever (the probed-services churn loop) instead
	// of backing off. RestartInstance carries Metadata.RestartCount to
	// the replacement, and the replacement's CreatedAt IS the moment of
	// that restart; restartInstanceWithBackoff forgives the history
	// after healthBackoffResetWindow of stable running.
	priorRestarts := 0
	if instance.Metadata != nil {
		priorRestarts = instance.Metadata.RestartCount
	}
	lastRestart := time.Time{} // zero = no prior restarts
	if priorRestarts > 0 {
		lastRestart = instance.CreatedAt
	}

	// Create a health state entry for the instance
	healthState := &instanceHealth{
		instance:            instance,
		service:             service,
		livenessResults:     make([]types.HealthCheckResult, 0),
		readinessResults:    make([]types.HealthCheckResult, 0),
		lastCheck:           time.Now(),
		consecutiveFailures: 0,
		healthRestartCount:  priorRestarts,
		lastRestartTime:     lastRestart,
	}

	// If no health checks are configured, consider the instance healthy by default
	if service.Health == nil {
		c.logger.Debug("No health checks configured for service, marking as healthy by default",
			log.Str("service", service.Name),
			log.Str("instance", instance.ID))

		// Mark as healthy by default
		healthState.livenessStatus = true
		healthState.readinessStatus = true

		// Add instance to monitored instances
		c.instances[instance.ID] = healthState
		return nil
	}

	// For instances with health checks, start as unhealthy until proven healthy
	healthState.livenessStatus = false
	healthState.readinessStatus = false

	// Add instance to monitored instances
	c.instances[instance.ID] = healthState

	c.startMonitorLocked(healthState, instance.ID)
	return nil
}

// startMonitorLocked spawns monitorInstance. Caller must hold c.mu.
func (c *healthController) startMonitorLocked(ih *instanceHealth, instanceID string) {
	if ih.monitoring {
		return
	}
	ih.monitoring = true
	c.wg.Add(1)
	go func() {
		defer func() {
			c.mu.Lock()
			if cur, ok := c.instances[instanceID]; ok {
				cur.monitoring = false
			}
			c.mu.Unlock()
			c.wg.Done()
		}()
		c.monitorInstance(instanceID)
	}()
}

// RemoveInstance removes an instance from monitoring
func (c *healthController) RemoveInstance(instanceID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Info("Removing instance from health monitoring",
		log.Str("instance", instanceID))

	// Check if instance is being monitored
	if _, exists := c.instances[instanceID]; !exists {
		return nil
	}

	// Remove instance from monitored instances
	delete(c.instances, instanceID)

	return nil
}

// GetHealthStatus gets the current health status of an instance
func (c *healthController) GetHealthStatus(ctx context.Context, instanceID string) (*types.InstanceHealthStatus, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check if instance is being monitored
	ih, exists := c.instances[instanceID]
	if !exists {
		// Instead of returning an error, return a default healthy status
		// This means the instance doesn't have any health checks configured
		// or hasn't been added to monitoring yet
		c.logger.Debug("Instance not being monitored, returning default healthy status",
			log.Str("instance", instanceID))

		return &types.InstanceHealthStatus{
			InstanceID:  instanceID,
			Liveness:    true, // Default to healthy
			Readiness:   true, // Default to ready
			LastChecked: time.Now(),
		}, nil
	}

	return &types.InstanceHealthStatus{
		InstanceID:  instanceID,
		Liveness:    ih.livenessStatus,
		Readiness:   ih.readinessStatus,
		LastChecked: ih.lastCheck,
	}, nil
}

// monitorInstance monitors the health of an instance
func (c *healthController) monitorInstance(instanceID string) {
	c.logger.Debug("Starting health monitoring for instance",
		log.Str("instance", instanceID))

	// Get instance state under read lock
	c.mu.RLock()
	ih, exists := c.instances[instanceID]
	if !exists {
		c.mu.RUnlock()
		c.logger.Error("Instance not found for monitoring, stopping", log.Str("instance", instanceID))
		return
	}

	// Get health check configurations
	service := ih.service
	if service == nil {
		c.mu.RUnlock()
		c.logger.Error("Service is nil for instance, stopping monitoring", log.Str("instance", instanceID))
		return
	}

	// If service has no health checks, we don't need to monitor it
	// (it's already marked as healthy in AddInstance)
	if service.Health == nil {
		c.mu.RUnlock()
		c.logger.Debug("Instance has no health checks configured, already marked healthy",
			log.Str("instance", instanceID))
		return
	}

	livenessProbe := service.Health.Liveness
	readinessProbe := service.Health.Readiness
	c.mu.RUnlock()

	// Configure check intervals with sensible defaults
	livenessInterval := 10 * time.Second
	readinessInterval := 10 * time.Second
	livenessInitialDelay := 0 * time.Second
	readinessInitialDelay := 0 * time.Second

	if livenessProbe != nil && livenessProbe.IntervalSeconds > 0 {
		livenessInterval = time.Duration(livenessProbe.IntervalSeconds) * time.Second
	}
	if readinessProbe != nil && readinessProbe.IntervalSeconds > 0 {
		readinessInterval = time.Duration(readinessProbe.IntervalSeconds) * time.Second
	}
	if livenessProbe != nil && livenessProbe.InitialDelaySeconds > 0 {
		livenessInitialDelay = time.Duration(livenessProbe.InitialDelaySeconds) * time.Second
	}
	if readinessProbe != nil && readinessProbe.InitialDelaySeconds > 0 {
		readinessInitialDelay = time.Duration(readinessProbe.InitialDelaySeconds) * time.Second
	}

	// Start the readiness monitoring goroutine BEFORE waiting on the
	// liveness initial delay. Each probe owns its own initial-delay
	// sleep, and they must run concurrently. Previously this goroutine
	// was spawned only after the liveness time.Sleep below, which made
	// the readiness probe's effective initial delay
	// livenessInitialDelay + readinessInitialDelay. With equal delays
	// (e.g. both 120s) the readiness probe did not start for 240s, so
	// any instance whose liveness probe failed it within that window
	// was restarted before its readiness probe ever ran — leaving the
	// instance stuck in Starting (readiness never promotes it).
	// Wait briefly for Start() to install the controller context. A
	// reconcile tick can call AddInstance before Start() finishes if
	// controllers start in the wrong order (fixed in orchestrator, but
	// keep this guard for tests and mis-ordered wiring).
	monitorCtx := c.waitForMonitorContext(30 * time.Second)
	if monitorCtx == nil {
		c.logger.Warn("Health monitor exiting: controller context never became ready",
			log.Str("instance", instanceID))
		return
	}

	if readinessProbe != nil {
		// Create a goroutine for readiness checks
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			if !sleepUntilDone(monitorCtx, readinessInitialDelay) {
				return
			}
			// First probe right after the initial delay — don't wait an
			// extra interval (ticker's first fire is one period later).
			c.performHealthCheck(instanceID, readinessProbe, "readiness")
			readinessTicker := time.NewTicker(readinessInterval)
			defer readinessTicker.Stop()

			for {
				// Check if context is nil before using it
				c.ctxMu.RLock()
				ctx := c.ctx
				c.ctxMu.RUnlock()

				if ctx == nil {
					c.logger.Debug("Context is nil, stopping readiness monitoring",
						log.Str("instance", instanceID))
					return
				}

				select {
				case <-ctx.Done():
					return
				case <-readinessTicker.C:
					if readinessProbe != nil {
						c.performHealthCheck(instanceID, readinessProbe, "readiness")
					}
				}
			}
		}()
	}

	// Main goroutine for liveness checks. Wait for the liveness
	// initial delay before starting checks.
	if !sleepUntilDone(monitorCtx, livenessInitialDelay) {
		return
	}
	if livenessProbe != nil {
		c.performHealthCheck(instanceID, livenessProbe, "liveness")
	}
	livenessTicker := time.NewTicker(livenessInterval)
	defer livenessTicker.Stop()
	for {
		// Check if context is nil before using it
		c.ctxMu.RLock()
		ctx := c.ctx
		c.ctxMu.RUnlock()

		if ctx == nil {
			c.logger.Debug("Context is nil, stopping liveness monitoring",
				log.Str("instance", instanceID))
			return
		}

		select {
		case <-ctx.Done():
			c.logger.Debug("Stopping health monitoring for instance",
				log.Str("instance", instanceID))
			return
		case <-livenessTicker.C:
			if livenessProbe != nil {
				c.performHealthCheck(instanceID, livenessProbe, "liveness")
			}
		}
	}
}

// performHealthCheck performs a health check for an instance
func (c *healthController) performHealthCheck(instanceID string, probe *types.Probe, checkType string) {
	start := time.Now()

	// Get instance under read lock
	c.mu.RLock()
	ih, exists := c.instances[instanceID]
	if !exists {
		c.mu.RUnlock()
		return
	}
	instance := ih.instance
	if instance == nil {
		c.mu.RUnlock()
		c.logger.Error("Instance is nil in health state, stopping health check", log.Str("instance", instanceID))
		return
	}
	c.mu.RUnlock()

	// Refresh from store so we stop probing tombstones / deleted
	// records that still have a monitor goroutine attached. Done
	// outside c.mu: store.Get does I/O, and the refreshed instance is
	// written back to ih.instance under the write lock — mutating it
	// under the read lock would race concurrent liveness/readiness
	// probes for the same instance.
	if c.store != nil {
		c.ctxMu.RLock()
		ctx := c.ctx
		c.ctxMu.RUnlock()
		if ctx != nil {
			var fresh types.Instance
			if err := c.store.Get(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, &fresh); err == nil {
				if healthMonitoringTerminal(fresh.Status) {
					_ = c.RemoveInstance(instanceID)
					return
				}
				c.mu.Lock()
				ih.instance = &fresh
				c.mu.Unlock()
				instance = &fresh
			}
		}
	}

	// Create a prober for this probe type
	prober, err := probes.NewProber(probe.Type)
	if err != nil {
		// Log unknown probe type error
		c.logger.Error("Failed to create prober",
			log.Str("instance", instanceID),
			log.Str("probe_type", probe.Type),
			log.Err(err))

		// Record failure result
		result := types.HealthCheckResult{
			Success:    false,
			Message:    fmt.Sprintf("Unknown health check type: %s", probe.Type),
			Duration:   time.Since(start),
			CheckTime:  time.Now(),
			InstanceID: instanceID,
			CheckType:  checkType,
		}

		c.updateHealthStatus(instanceID, result, checkType)
		return
	}

	// Check if context is nil before creating probe context
	c.ctxMu.RLock()
	ctx := c.ctx
	c.ctxMu.RUnlock()

	if ctx == nil {
		c.logger.Debug("Context is nil, skipping health check",
			log.Str("instance", instanceID),
			log.Str("check_type", checkType))
		return
	}

	// Create probe context with all necessary dependencies
	probeCtx := &probes.ProbeContext{
		Ctx:            ctx,
		Logger:         c.logger,
		Instance:       instance,
		ProbeConfig:    probe,
		HTTPClient:     c.client,
		RunnerProvider: c, // Health controller implements RunnerProvider
	}

	// Execute the appropriate probe
	probeResult := prober.Execute(probeCtx)

	// Record the result
	result := types.HealthCheckResult{
		Success:    probeResult.Success,
		Message:    probeResult.Message,
		Duration:   probeResult.Duration,
		CheckTime:  time.Now(),
		InstanceID: instanceID,
		CheckType:  checkType,
	}

	// Update instance health status
	c.updateHealthStatus(instanceID, result, checkType)
}

// updateHealthStatus updates the health status based on a check result
func (c *healthController) updateHealthStatus(instanceID string, result types.HealthCheckResult, checkType string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if instance still exists
	ih, exists := c.instances[instanceID]
	if !exists {
		return
	}

	// Update status based on check type
	switch checkType {
	case "liveness":
		ih.livenessResults = append(ih.livenessResults, result)
		if len(ih.livenessResults) > 10 {
			ih.livenessResults = ih.livenessResults[1:]
		}

		// Update liveness status based on success
		ih.livenessStatus = result.Success

		// Log the result
		if result.Success {
			c.logger.Debug("Liveness check passed",
				log.Str("instance", instanceID),
				log.Duration("duration", result.Duration))

			// Reset failure counter on success
			ih.consecutiveFailures = 0
		} else {
			// Increment consecutive failures counter
			ih.consecutiveFailures++

			c.logger.Warn("Liveness check failed",
				log.Str("instance", instanceID),
				log.Str("message", result.Message),
				log.Int("consecutive_failures", ih.consecutiveFailures),
				log.Duration("duration", result.Duration))

			// Trigger a restart once the probe's configured
			// failureThreshold is reached (default 3). This was
			// hardcoded to 3, silently ignoring the user's
			// `failureThreshold:` — services tuned for slow starts
			// restarted two probes early.
			if ih.consecutiveFailures >= livenessFailureThreshold(ih.service) {
				if err := c.restartInstanceWithBackoff(instanceID, ih); err != nil {
					if errors.Is(err, errRestartBackoff) {
						c.logger.Debug("Restart deferred by CrashLoopBackoff",
							log.Str("instance", instanceID),
							log.Err(err))
					} else {
						c.logger.Error("Failed to restart unhealthy instance",
							log.Str("instance", instanceID),
							log.Err(err))
					}
				}
			}
		}
	case "readiness":
		ih.readinessResults = append(ih.readinessResults, result)
		if len(ih.readinessResults) > 10 {
			ih.readinessResults = ih.readinessResults[1:]
		}

		ih.readinessStatus = result.Success
		// A successful readiness pass promotes the instance from
		// Starting to Running. Fire-and-forget so the store I/O
		// doesn't block the c.mu we're currently holding (otherwise
		// a slow store write would stall every other health update
		// across the process). promoteToRunningOnReady is idempotent
		// — it only flips when Status is still Starting — so a
		// late-arriving goroutine is safe.
		//
		// We retry on every readiness pass until promotion actually
		// succeeds (ih.promoted), rather than firing only on the
		// first pass: a single transient failure (store contention,
		// the record briefly absent) must not leave the instance
		// wedged in Starting forever.
		if result.Success && !ih.promoted {
			ih.promoted = true // optimistic; reset below if it fails
			namespace := ""
			if ih.instance != nil {
				namespace = ih.instance.Namespace
			}
			go func(ns, id string) {
				if err := c.promoteToRunningOnReady(ns, id); err != nil {
					c.mu.Lock()
					if ih2, ok := c.instances[id]; ok {
						ih2.promoted = false // allow the next readiness pass to retry
					}
					c.mu.Unlock()
					c.logger.Warn("Failed to promote instance to Running on readiness pass",
						log.Str("instance", id),
						log.Err(err))
				}
			}(namespace, instanceID)
		}

		if result.Success {
			c.logger.Debug("Readiness check passed",
				log.Str("instance", instanceID),
				log.Duration("duration", result.Duration))
		} else {
			c.logger.Warn("Readiness check failed",
				log.Str("instance", instanceID),
				log.Str("message", result.Message),
				log.Duration("duration", result.Duration))
		}
	}

	ih.lastCheck = time.Now()
}

// promoteToRunningOnReady flips an instance's Status from Starting to
// Running on the first successful readiness probe. Loads the current
// record (so we don't overwrite concurrent updates from the
// reconciler) and only writes when the instance is still in the
// Starting state we left it in — otherwise the reconciler may have
// already moved it (Failed, Stopped, Deleted) and we have no business
// resurrecting it. Best-effort; called under the instanceHealth lock
// but operates on the store independently.
func (c *healthController) promoteToRunningOnReady(namespace, instanceID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Flip Starting→Running atomically on the freshly-read instance. UpdateFunc's
	// CAS closes the read→write window the old GetInstanceByID+Update left open:
	// a reconciler instance write can no longer be clobbered by this promotion,
	// nor can it clobber ours (RFC #129 Phase 1c). The Starting guard becomes an
	// ErrSkipUpdate so we never resurrect an instance the reconciler already
	// moved to Failed/Stopped/Deleted.
	var current types.Instance
	promoted := false
	err := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, namespace, instanceID, &current, func() error {
		promoted = false // reset per attempt (mutate may re-run on conflict)
		if current.Status != types.InstanceStatusStarting {
			return store.ErrSkipUpdate
		}
		now := time.Now()
		current.Status = types.InstanceStatusRunning
		current.StatusMessage = "Ready"
		current.UpdatedAt = now
		// Stamp the transition time too. This path does not go through
		// applyInstanceStatus (the only other writer of LastTransitionAt),
		// so without this the field still marks the *Starting* transition —
		// and the update planner's minimum-ready window, which measures
		// "how long has this held Running", would be measured from container
		// start instead of from readiness. That silently defeats the gate
		// for exactly the probe-gated services it matters most for
		// (RUNE-042 §8.5).
		current.LastTransitionAt = &now
		promoted = true
		return nil
	}, store.WithHealthController())
	if err != nil {
		return fmt.Errorf("promote instance to running: %w", err)
	}
	if !promoted {
		// Instance had already moved on — nothing to republish.
		return nil
	}
	// Republish endpoints so the data plane (load balancer / DNS)
	// picks up the now-Running instance. Without this, the instance
	// is correctly marked Running but stays invisible to traffic
	// until the next event that triggers a republish (e.g. service
	// update). Nil-safe when no publisher is wired.
	c.instanceController.RepublishServiceByInstance(ctx, &current)
	c.logger.Info("Instance promoted to Running on first readiness pass",
		log.Str("instance", instanceID))
	return nil
}

// errRestartBackoff marks a restart deferred by CrashLoopBackoff — a
// normal pacing condition, not a failure. Callers match with errors.Is.
var errRestartBackoff = errors.New("restart deferred by backoff")

// healthBackoffResetWindow is how long a slot must run since its last
// restart before its backoff history is forgiven (K8s CrashLoopBackoff
// resets after 10 minutes of successful running). Failing again INSIDE
// the window means the slot is still crash-looping — escalate; failing
// after it is a fresh incident — start over at the base backoff.
const healthBackoffResetWindow = 10 * time.Minute

// livenessFailureThreshold returns the service's configured liveness
// failureThreshold, defaulting to 3 (K8s default) when unset.
func livenessFailureThreshold(service *types.Service) int {
	if service != nil && service.Health != nil && service.Health.Liveness != nil &&
		service.Health.Liveness.FailureThreshold > 0 {
		return service.Health.Liveness.FailureThreshold
	}
	return 3
}

// restartInstanceWithBackoff restarts an instance with exponential backoff
// Note on timing: the restart DECISION (backoff eligibility, counters, removal
// from the monitor map) is synchronous and happens under the caller's lock; the
// teardown itself is dispatched to a goroutine. Callers and tests must
// therefore wait for the effect rather than assume it landed before this
// returns. See the goroutine below for why.
func (c *healthController) restartInstanceWithBackoff(instanceID string, ih *instanceHealth) error {
	// Get the current time
	now := time.Now()

	// A slot that ran healthy past the reset window since its last
	// restart is a fresh incident, not a continuing crash loop.
	if ih.healthRestartCount > 0 && !ih.lastRestartTime.IsZero() &&
		now.Sub(ih.lastRestartTime) > healthBackoffResetWindow {
		ih.healthRestartCount = 0
	}

	// Calculate the backoff duration based on health restart count.
	// Base backoff is 10 seconds, doubles each restart up to a max of
	// 5 minutes. The shift is capped BEFORE the multiply: the count is
	// seeded from the slot's persisted RestartCount, so large values
	// would otherwise overflow the duration.
	shift := utils.ToUintNonNegative(ih.healthRestartCount)
	if shift > 5 {
		shift = 5 // 10s<<5 = 320s, already past maxBackoff
	}
	backoff := 10 * time.Second * time.Duration(1<<shift)
	maxBackoff := 5 * time.Minute
	if backoff > maxBackoff {
		backoff = maxBackoff
	}

	// Check if enough time has elapsed since the last restart
	if !ih.lastRestartTime.IsZero() && now.Sub(ih.lastRestartTime) < backoff {
		// Not enough time has elapsed, skip this restart
		return fmt.Errorf("%w: next eligible in %v",
			errRestartBackoff, backoff-(now.Sub(ih.lastRestartTime)))
	}

	// Log the restart attempt
	c.logger.Info("Restarting unhealthy instance",
		log.Str("instance", instanceID),
		log.Int("health_restart_count", ih.healthRestartCount+1),
		log.Duration("backoff", backoff))

	// Get the instance
	instance := ih.instance
	if instance == nil {
		return fmt.Errorf("instance is nil, cannot restart")
	}

	// Get context safely (may be nil if controller is stopping)
	c.ctxMu.RLock()
	ctx := c.ctx
	c.ctxMu.RUnlock()

	if ctx == nil {
		return fmt.Errorf("health controller is stopping, cannot restart instance")
	}

	// Drop the failing record from the monitor map BEFORE handing the
	// restart off. The caller (updateHealthStatus) holds c.mu, so this must
	// be an inline delete rather than RemoveInstance (which would deadlock).
	// Doing it first also means the restart goroutine below cannot race a
	// probe against a slot we are about to replace.
	delete(c.instances, instanceID)
	c.logger.Info("Removing instance from health monitoring",
		log.Str("instance", instanceID))

	// Run the restart OFF the lock.
	//
	// c.mu is the single lock for every instance's health state, and
	// RestartInstance is no longer cheap: since RUNE-042 Phase 0 it withdraws
	// the instance from the dataplane and then DRAINS — a blocking sleep of
	// drainSeconds (5s by default, up to an hour if configured) — before
	// stopping the container. Holding c.mu across that freezes every other
	// instance's health update, AddInstance and RemoveInstance node-wide;
	// and because the reconciler calls RemoveInstance before each retire,
	// it stalls all four reconcile workers too. One flapping container
	// would halt health monitoring and reconciliation for the whole node.
	//
	// This is the same reasoning that already made promoteToRunningOnReady
	// fire-and-forget (see updateHealthStatus): slow work does not belong
	// under this lock. Fire-and-forget is safe here because every mutation
	// of ih and c.instances is already done above.
	go func(inst *types.Instance) {
		if err := c.instanceController.RestartInstance(ctx, inst, instancectl.RestartReasonHealthCheckFailure); err != nil {
			c.logger.Error("Failed to restart unhealthy instance",
				log.Str("instance", instanceID),
				log.Err(err))
		}
	}(instance)

	// Update health restart metrics
	ih.healthRestartCount++
	ih.lastRestartTime = now
	ih.consecutiveFailures = 0 // Reset failures after restart

	c.logger.Info("Instance restart initiated successfully",
		log.Str("instance", instanceID),
		log.Int("health_restart_count", ih.healthRestartCount))

	return nil
}

// healthMonitoringTerminal is true for instance statuses that must not
// receive further liveness/readiness probes.
func (c *healthController) waitForMonitorContext(maxWait time.Duration) context.Context {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		c.ctxMu.RLock()
		ctx := c.ctx
		c.ctxMu.RUnlock()
		if ctx != nil {
			return ctx
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// sleepUntilDone waits for d unless the health controller context is
// cancelled (Ctrl+C / Stop). Returns false when shutdown was requested.
func sleepUntilDone(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func healthMonitoringTerminal(s types.InstanceStatus) bool {
	switch s {
	case types.InstanceStatusFailed,
		types.InstanceStatusDeleted,
		types.InstanceStatusStalled,
		types.InstanceStatusExited:
		return true
	default:
		return false
	}
}

// Compile-time proof the real controller satisfies this consumer's slice.
var _ healthInstanceOps = (*instancectl.Controller)(nil)
