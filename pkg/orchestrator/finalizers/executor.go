package finalizers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/worker"
)

// FinalizerExecutor executes finalizers with dependency resolution, timeouts, and retries
type FinalizerExecutor struct {
	registry      *FinalizerRegistry
	logger        log.Logger
	timeoutConfig types.FinalizerTimeoutConfig
	retryPolicy   worker.RetryPolicy
}

// NewFinalizerExecutor creates a new finalizer executor
func NewFinalizerExecutor(
	registry *FinalizerRegistry,
	logger log.Logger,
	timeoutConfig types.FinalizerTimeoutConfig,
	retryPolicy worker.RetryPolicy,
) *FinalizerExecutor {
	return &FinalizerExecutor{
		registry:      registry,
		logger:        logger,
		timeoutConfig: timeoutConfig,
		retryPolicy:   retryPolicy,
	}
}

// DefaultFinalizerExecutor creates a finalizer executor with sensible defaults
// and automatically registers common finalizers
func DefaultFinalizerExecutor(
	storeInstance store.Store,
	instanceController types.InstanceFinalizerInterface,
	healthController types.HealthFinalizerInterface,
	logger log.Logger,
) *FinalizerExecutor {
	// Create finalizer registry and register common finalizers
	registry := NewFinalizerRegistry()

	// Register instance cleanup finalizer
	registry.Register(types.FinalizerTypeInstanceCleanup, func() FinalizerInterface {
		return NewInstanceCleanupFinalizer(storeInstance, instanceController, healthController, logger)
	})

	// Register service deregister finalizer
	registry.Register(types.FinalizerTypeServiceDeregister, func() FinalizerInterface {
		return NewServiceDeregisterFinalizer(storeInstance, logger)
	})

	// Default timeout configuration
	timeoutConfig := types.FinalizerTimeoutConfig{
		DefaultTimeout: 5 * time.Minute,
		TypeTimeouts: map[types.FinalizerType]time.Duration{
			types.FinalizerTypeInstanceCleanup:   2 * time.Minute,
			types.FinalizerTypeServiceDeregister: 30 * time.Second,
		},
		FailOnTimeout: true,
	}

	// Default retry policy
	retryPolicy := worker.RetryPolicy{
		MaxAttempts:       3,
		InitialDelay:      time.Second,
		MaxDelay:          time.Minute,
		BackoffMultiplier: 2.0,
	}

	return NewFinalizerExecutor(registry, logger, timeoutConfig, retryPolicy)
}

// FinalizerEvent represents an event emitted during finalizer execution
type FinalizerEvent struct {
	FinalizerID   string
	FinalizerType types.FinalizerType
	Status        types.FinalizerStatus
	Error         string
	Timestamp     time.Time
}

// ExecuteFinalizers executes a list of finalizers in dependency order
// It returns a channel that emits events for real-time progress tracking
func (e *FinalizerExecutor) ExecuteFinalizers(ctx context.Context, service *types.Service, finalizerTypes []types.FinalizerType) (<-chan FinalizerEvent, error) {
	// Create event channel
	eventChan := make(chan FinalizerEvent, 100) // Buffered channel to avoid blocking

	// Start execution in a goroutine
	go func() {
		defer close(eventChan) // Close channel when done

		// Create finalizer instances
		finalizers := make(map[types.FinalizerType]FinalizerInterface)
		for _, finalizerType := range finalizerTypes {
			finalizer, err := e.registry.Create(finalizerType)
			if err != nil {
				e.logger.Error("Failed to create finalizer",
					log.Str("finalizer", string(finalizerType)), log.Err(err))
				return
			}
			finalizers[finalizerType] = finalizer
		}

		// Resolve execution order
		orderedTypes, err := e.resolveExecutionOrder(finalizerTypes, finalizers)
		if err != nil {
			e.logger.Error("Failed to resolve finalizer order", log.Err(err))
			return
		}

		e.logger.Info("Executing finalizers in order",
			log.Str("service", service.Name),
			log.Str("namespace", service.Namespace),
			log.Int("count", len(orderedTypes)))

		// Execute finalizers in order
		for _, finalizerType := range orderedTypes {
			if err := e.executeFinalizer(ctx, finalizers[finalizerType], finalizerType, service, eventChan); err != nil {
				if e.timeoutConfig.FailOnTimeout {
					return
				}
				e.logger.Error("Finalizer failed but continuing", log.Err(err))
			}
		}
	}()

	return eventChan, nil
}

// executeFinalizer executes a single finalizer and emits events
func (e *FinalizerExecutor) executeFinalizer(ctx context.Context, finalizer FinalizerInterface, finalizerType types.FinalizerType, service *types.Service, eventChan chan<- FinalizerEvent) error {
	finalizerID := fmt.Sprintf("%s-%d", string(finalizerType), time.Now().Unix())

	e.logger.Info("Executing finalizer", log.Str("finalizer", string(finalizerType)))

	// Set status and emit running event
	finalizer.SetStatus(types.FinalizerStatusRunning)
	e.emitEvent(eventChan, FinalizerEvent{
		FinalizerID:   finalizerID,
		FinalizerType: finalizerType,
		Status:        types.FinalizerStatusRunning,
		Timestamp:     time.Now(),
	})

	// Execute with timeout and retry
	if err := e.executeWithTimeoutAndRetry(ctx, finalizer, service); err != nil {
		// Handle failure
		finalizer.SetStatus(types.FinalizerStatusFailed)
		finalizer.SetError(err.Error())

		e.emitEvent(eventChan, FinalizerEvent{
			FinalizerID:   finalizerID,
			FinalizerType: finalizerType,
			Status:        types.FinalizerStatusFailed,
			Error:         err.Error(),
			Timestamp:     time.Now(),
		})

		return fmt.Errorf("finalizer %s failed: %w", finalizerType, err)
	}

	// Handle success
	finalizer.SetStatus(types.FinalizerStatusCompleted)
	finalizer.SetCompletedAt(time.Now())

	e.emitEvent(eventChan, FinalizerEvent{
		FinalizerID:   finalizerID,
		FinalizerType: finalizerType,
		Status:        types.FinalizerStatusCompleted,
		Timestamp:     time.Now(),
	})

	e.logger.Info("Finalizer completed successfully", log.Str("finalizer", string(finalizerType)))
	return nil
}

// emitEvent safely emits an event to the channel without blocking
func (e *FinalizerExecutor) emitEvent(eventChan chan<- FinalizerEvent, event FinalizerEvent) {
	if eventChan == nil {
		return
	}

	select {
	case eventChan <- event:
	default: // Don't block if channel is full
	}
}

// resolveExecutionOrder resolves the execution order based on dependencies
func (e *FinalizerExecutor) resolveExecutionOrder(
	finalizerTypes []types.FinalizerType,
	finalizers map[types.FinalizerType]FinalizerInterface,
) ([]types.FinalizerType, error) {
	// Create a graph of finalizer dependencies
	graph := make(map[types.FinalizerType][]types.FinalizerType)
	inDegree := make(map[types.FinalizerType]int)

	// Initialize in-degree for all finalizers
	for _, finalizerType := range finalizerTypes {
		inDegree[finalizerType] = 0
	}

	// Build the dependency graph
	for _, finalizerType := range finalizerTypes {
		finalizer := finalizers[finalizerType]
		for _, dep := range finalizer.GetDependencies() {
			if dep.Required {
				// Check if dependency is in our list
				found := false
				for _, ft := range finalizerTypes {
					if ft == dep.DependsOn {
						found = true
						break
					}
				}
				if found {
					graph[dep.DependsOn] = append(graph[dep.DependsOn], finalizerType)
					inDegree[finalizerType]++
				}
			}
		}
	}

	// Topological sort to determine execution order
	var order []types.FinalizerType
	var queue []types.FinalizerType

	// Add finalizers with no dependencies to the queue
	for _, finalizerType := range finalizerTypes {
		if inDegree[finalizerType] == 0 {
			queue = append(queue, finalizerType)
		}
	}

	// Process the queue
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		order = append(order, current)

		// Update dependencies
		for _, dependent := range graph[current] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	// Check for cycles
	if len(order) != len(finalizerTypes) {
		return nil, fmt.Errorf("circular dependency detected in finalizers")
	}

	return order, nil
}

// executeWithTimeoutAndRetry executes a finalizer with timeout and retry logic
func (e *FinalizerExecutor) executeWithTimeoutAndRetry(ctx context.Context, finalizer FinalizerInterface, service *types.Service) error {
	// Get timeout for this finalizer type
	timeout := e.timeoutConfig.DefaultTimeout
	if typeTimeout, ok := e.timeoutConfig.TypeTimeouts[finalizer.GetType()]; ok {
		timeout = typeTimeout
	}

	// Execute with retry
	return e.executeWithRetry(ctx, finalizer, service, timeout)
}

// executeWithRetry executes a finalizer with retry logic
func (e *FinalizerExecutor) executeWithRetry(ctx context.Context, finalizer FinalizerInterface, service *types.Service, timeout time.Duration) error {
	var lastErr error
	delay := e.retryPolicy.InitialDelay

	for attempt := 0; attempt < e.retryPolicy.MaxAttempts; attempt++ {
		if attempt > 0 {
			e.logger.Info("Retrying finalizer",
				log.Str("finalizer", string(finalizer.GetType())),
				log.Int("attempt", attempt+1),
				log.Duration("delay", delay))

			// Wait before retry
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}

			// Calculate next delay with exponential backoff
			delay = time.Duration(float64(delay) * e.retryPolicy.BackoffMultiplier)
			if delay > e.retryPolicy.MaxDelay {
				delay = e.retryPolicy.MaxDelay
			}
		}

		// Create timeout context
		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)

		// Execute finalizer with timeout
		errChan := make(chan error, 1)
		go func() {
			defer cancel()
			errChan <- finalizer.Execute(timeoutCtx, service)
		}()

		select {
		case err := <-errChan:
			if err == nil {
				return nil
			}
			lastErr = err
		case <-timeoutCtx.Done():
			cancel()
			lastErr = fmt.Errorf("finalizer %s timed out after %v", finalizer.GetType(), timeout)
		}

		// Check if we should retry
		if !e.shouldRetry(lastErr) {
			return lastErr
		}
	}

	return fmt.Errorf("finalizer %s failed after %d attempts: %w",
		finalizer.GetType(), e.retryPolicy.MaxAttempts, lastErr)
}

// shouldRetry determines if an error is retryable
func (e *FinalizerExecutor) shouldRetry(err error) bool {
	// Check if error is retryable
	if errors.Is(err, context.DeadlineExceeded) {
		// For now, always retry timeout errors
		// This could be made configurable in the future
		return true
	}

	// Add other retryable error conditions here
	// For now, retry all errors except context cancellation
	return !errors.Is(err, context.Canceled)
}
