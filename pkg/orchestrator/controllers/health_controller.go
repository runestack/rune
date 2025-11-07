package controllers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/probes"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// HealthController monitors instance health
type HealthController interface {
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

// healthController implements the HealthController interface
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
	instanceController InstanceController
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
	lastCheck           time.Time
	consecutiveFailures int
	healthRestartCount  int // Separate count for health check restarts (backoff calculation)
	lastRestartTime     time.Time
}

// NewHealthController creates a new health controller
func NewHealthController(logger log.Logger, store store.Store, runnerManager manager.IRunnerManager, instanceController InstanceController) HealthController {
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

	// Wait for all goroutines to finish
	c.wg.Wait()

	return nil
}

// AddInstance adds an instance to be monitored
func (c *healthController) AddInstance(service *types.Service, instance *types.Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Info("Adding instance to health monitoring",
		log.Str("instance", instance.ID))

	// Check if instance is already being monitored
	if _, exists := c.instances[instance.ID]; exists {
		c.logger.Debug("Instance already being monitored",
			log.Str("instance", instance.ID))
		return nil
	}

	// Create a health state entry for the instance
	healthState := &instanceHealth{
		instance:            instance,
		service:             service,
		livenessResults:     make([]types.HealthCheckResult, 0),
		readinessResults:    make([]types.HealthCheckResult, 0),
		lastCheck:           time.Now(),
		consecutiveFailures: 0,
		healthRestartCount:  0,           // Health restart count starts at 0
		lastRestartTime:     time.Time{}, // Zero time represents no prior restarts
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

	// Start monitoring goroutine for this instance
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.monitorInstance(instance.ID)
	}()

	return nil
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

	// Create tickers for each probe type
	var livenessTicker, readinessTicker *time.Ticker

	// Wait for initial delays before starting checks
	time.Sleep(livenessInitialDelay)
	livenessTicker = time.NewTicker(livenessInterval)

	if readinessProbe != nil {
		// Create a goroutine for readiness checks
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			time.Sleep(readinessInitialDelay)
			readinessTicker = time.NewTicker(readinessInterval)
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

	// Main goroutine for liveness checks
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

			// Trigger a restart if we have had enough failures
			// (typically 3-5 consecutive failures)
			if ih.consecutiveFailures >= 3 {
				if err := c.restartInstanceWithBackoff(instanceID, ih); err != nil {
					c.logger.Error("Failed to restart unhealthy instance",
						log.Str("instance", instanceID),
						log.Err(err))
				}
			}
		}
	case "readiness":
		ih.readinessResults = append(ih.readinessResults, result)
		if len(ih.readinessResults) > 10 {
			ih.readinessResults = ih.readinessResults[1:]
		}

		// Update readiness status based on success
		ih.readinessStatus = result.Success

		// Log the result
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

// restartInstanceWithBackoff restarts an instance with exponential backoff
func (c *healthController) restartInstanceWithBackoff(instanceID string, ih *instanceHealth) error {
	// Get the current time
	now := time.Now()

	// Calculate the backoff duration based on health restart count
	// Base backoff is 10 seconds, doubles each restart up to a max of 5 minutes
	backoff := 10 * time.Second * time.Duration(1<<utils.ToUintNonNegative(ih.healthRestartCount))
	maxBackoff := 5 * time.Minute
	if backoff > maxBackoff {
		backoff = maxBackoff
	}

	// Check if enough time has elapsed since the last restart
	if !ih.lastRestartTime.IsZero() && now.Sub(ih.lastRestartTime) < backoff {
		// Not enough time has elapsed, skip this restart
		return fmt.Errorf("skipping restart due to backoff, next eligible in %v",
			backoff-(now.Sub(ih.lastRestartTime)))
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

	// Use the instance controller to handle the restart
	if err := c.instanceController.RestartInstance(ctx, instance, InstanceRestartReasonHealthCheckFailure); err != nil {
		return fmt.Errorf("failed to restart instance: %w", err)
	}

	// Update health restart metrics
	ih.healthRestartCount++
	ih.lastRestartTime = now
	ih.consecutiveFailures = 0 // Reset failures after restart

	c.logger.Info("Instance restart initiated successfully",
		log.Str("instance", instanceID),
		log.Int("health_restart_count", ih.healthRestartCount))

	return nil
}
