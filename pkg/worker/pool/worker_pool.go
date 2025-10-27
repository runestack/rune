package pool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/worker"
	"github.com/runestack/rune/pkg/worker/metrics"
	"github.com/runestack/rune/pkg/worker/queue"
	"github.com/runestack/rune/pkg/worker/store"
)

// QueueOptions defines the base options for all queue types
type QueueOptions struct {
	Capacity int
}

// PriorityQueueOptions defines options for a priority queue
type PriorityQueueOptions struct {
	Capacity int
}

// DelayedQueueOptions defines options for a delayed queue
type DelayedQueueOptions struct {
	Capacity int
}

// RateLimitQueueOptions defines options for a rate-limited queue
type RateLimitQueueOptions struct {
	Capacity      int
	RatePerSecond float64
}

// BatchQueueOptions defines options for a batch queue
type BatchQueueOptions struct {
	Capacity  int
	BatchSize int
}

// WorkerPoolConfig holds the configuration for a worker pool
type WorkerPoolConfig struct {
	// Name of the worker pool
	Name string

	// Number of workers in the pool
	NumWorkers int

	// Queue configuration
	QueueType      queue.QueueType
	QueueCapacity  int
	QueueRateLimit time.Duration
	QueueBatchSize int

	// Dead letter queue configuration
	DeadLetterQueueCapacity int

	// Default timeout for task execution
	DefaultTimeout time.Duration

	// Default retry policy
	DefaultRetryPolicy worker.RetryPolicy

	// Error handler for task execution errors
	ErrorHandler func(error)

	// Metrics configuration
	EnableMetrics bool
	Metrics       *metrics.Metrics

	// Task store for persistence (now includes execution results)
	TaskStore store.TaskStore

	// Enable task persistence
	EnablePersistence bool
}

// WorkerPool manages a pool of workers that process tasks
type WorkerPool struct {
	config          WorkerPoolConfig
	queue           queue.Queue
	deadLetterQueue queue.Queue
	wg              sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
	metrics         *metrics.Metrics
	taskStore       store.TaskStore
	// Track task submission times for latency calculation
	taskTimes sync.Map
}

// NewWorkerPool creates a new worker pool with the given configuration
func NewWorkerPool(config WorkerPoolConfig) (*WorkerPool, error) {
	if config.NumWorkers <= 0 {
		return nil, fmt.Errorf("number of workers must be greater than 0")
	}

	if config.QueueCapacity <= 0 {
		return nil, fmt.Errorf("queue capacity must be greater than 0")
	}

	// Set default dead letter queue capacity if not specified
	if config.DeadLetterQueueCapacity <= 0 {
		config.DeadLetterQueueCapacity = config.QueueCapacity / 10 // Default to 10% of main queue
		if config.DeadLetterQueueCapacity < 10 {
			config.DeadLetterQueueCapacity = 10 // Minimum of 10
		}
	}

	// Set up default task stores if persistence is enabled but stores not provided
	var taskStore store.TaskStore

	if config.EnablePersistence {
		if config.TaskStore == nil {
			taskStore = store.NewMemoryTaskStore()
		} else {
			taskStore = config.TaskStore
		}
	}

	var q queue.Queue
	switch config.QueueType {
	case queue.QueueTypePriority:
		q = queue.NewPriorityQueue(config.QueueCapacity)
	case queue.QueueTypeDelayed:
		q = queue.NewDelayedQueue(config.QueueCapacity)
	case queue.QueueTypeRateLimit:
		baseQueue := queue.NewPriorityQueue(config.QueueCapacity)
		q = queue.NewRateLimitQueue(baseQueue, config.QueueRateLimit, config.QueueCapacity)
	case queue.QueueTypeBatch:
		baseQueue := queue.NewPriorityQueue(config.QueueCapacity)
		q = queue.NewBatchQueue(baseQueue, config.QueueBatchSize, config.QueueCapacity)
	case queue.QueueTypeFIFO:
		q = queue.NewFIFOQueue(config.QueueCapacity)
	case queue.QueueTypeDeadLetter:
		q = queue.NewDeadLetterQueue(config.QueueCapacity)
	default:
		return nil, fmt.Errorf("unsupported queue type: %s", config.QueueType)
	}

	// Create dead letter queue
	deadLetterQueue := queue.NewDeadLetterQueue(config.DeadLetterQueueCapacity)

	ctx, cancel := context.WithCancel(context.Background())

	pool := &WorkerPool{
		config:          config,
		queue:           q,
		deadLetterQueue: deadLetterQueue,
		ctx:             ctx,
		cancel:          cancel,
		metrics:         metrics.NewMetrics(),
		taskStore:       taskStore,
	}

	// Start metrics collection if enabled
	if config.EnableMetrics {
		go pool.collectMetrics()
	}

	return pool, nil
}

// DefaultWorkerPool creates a worker pool with sensible defaults
func DefaultWorkerPool(name string, numWorkers int) (*WorkerPool, error) {
	if numWorkers <= 0 {
		numWorkers = 3 // Default to 3 workers
	}

	if name == "" {
		name = "default"
	}

	config := WorkerPoolConfig{
		Name:           name,
		NumWorkers:     numWorkers,
		QueueType:      queue.QueueTypePriority,
		QueueCapacity:  100,
		DefaultTimeout: 10 * time.Minute,
		DefaultRetryPolicy: worker.RetryPolicy{
			MaxAttempts:       3,
			InitialDelay:      time.Second,
			MaxDelay:          time.Minute,
			BackoffMultiplier: 2.0,
		},
		EnableMetrics:           true,
		EnablePersistence:       false,
		DeadLetterQueueCapacity: 10,
	}

	return NewWorkerPool(config)
}

// DeletionWorkerPool creates a worker pool optimized for deletion tasks
func DeletionWorkerPool(numWorkers int) (*WorkerPool, error) {
	if numWorkers <= 0 {
		numWorkers = 3 // Default to 3 workers for deletion tasks
	}

	config := WorkerPoolConfig{
		Name:           "deletion",
		NumWorkers:     numWorkers,
		QueueType:      queue.QueueTypePriority, // Priority queue for deletion urgency
		QueueCapacity:  50,                      // Smaller capacity for deletion tasks
		DefaultTimeout: 15 * time.Minute,        // Longer timeout for deletion operations
		DefaultRetryPolicy: worker.RetryPolicy{
			MaxAttempts:       5, // More retries for deletion tasks
			InitialDelay:      2 * time.Second,
			MaxDelay:          2 * time.Minute,
			BackoffMultiplier: 2.0,
		},
		EnableMetrics:           true,
		EnablePersistence:       false, // Keep simple for deletion tasks
		DeadLetterQueueCapacity: 5,     // Smaller DLQ for deletion tasks
	}

	return NewWorkerPool(config)
}

// collectMetrics periodically collects and updates metrics
func (p *WorkerPool) collectMetrics() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			// Update queue metrics
			size, err := p.queue.Size(p.ctx)
			if err == nil {
				// Keep the current latency value when updating queue size
				p.metrics.UpdateQueueMetrics(int64(size), time.Duration(p.metrics.QueueLatency.Load()))
			}

			// Update worker metrics
			active := p.metrics.TasksInProgress.Load()
			idle := int64(p.config.NumWorkers) - active
			p.metrics.UpdateWorkerMetrics(active, idle)
		}
	}
}

// Start starts the worker pool
func (p *WorkerPool) Start() {
	p.wg.Add(p.config.NumWorkers)
	for i := 0; i < p.config.NumWorkers; i++ {
		go p.runWorker(i)
	}
}

// Stop stops the worker pool
func (p *WorkerPool) Stop() {
	p.cancel()
	p.wg.Wait()
}

// Submit submits a task to the worker pool
func (p *WorkerPool) Submit(ctx context.Context, task worker.Task) error {
	// Validate task
	if err := task.Validate(); err != nil {
		return fmt.Errorf("invalid task: %w", err)
	}

	// Check for circular dependencies
	if err := p.CheckCircularDependencies(ctx, task); err != nil {
		return fmt.Errorf("circular dependency detected: %w", err)
	}

	// Validate dependencies
	if err := p.ValidateTaskDependencies(ctx, task); err != nil {
		return fmt.Errorf("dependency validation failed: %w", err)
	}

	// Save task to store if persistence is enabled
	if p.config.EnablePersistence && p.taskStore != nil {
		if err := p.taskStore.Save(ctx, task); err != nil {
			return fmt.Errorf("failed to save task to store: %w", err)
		}
	}

	// Update metrics
	p.metrics.TasksSubmitted.Add(1)
	p.metrics.TasksQueued.Add(1)

	// Store submission time for latency calculation
	p.taskTimes.Store(task.GetID(), time.Now())

	// Submit task to queue
	err := p.queue.Add(ctx, task)
	if err != nil {
		p.metrics.TasksQueued.Add(-1)
		// Remove from store if queue submission failed
		if p.config.EnablePersistence && p.taskStore != nil {
			p.taskStore.Delete(ctx, task.GetID())
		}
		return fmt.Errorf("failed to submit task: %w", err)
	}

	return nil
}

// SubmitAfter submits a task to be executed after a delay
func (p *WorkerPool) SubmitAfter(ctx context.Context, task worker.Task, delay time.Duration) error {
	// Validate task
	if err := task.Validate(); err != nil {
		return fmt.Errorf("invalid task: %w", err)
	}

	// Update metrics
	p.metrics.TasksSubmitted.Add(1)
	p.metrics.TasksQueued.Add(1)

	// Store submission time for latency calculation
	p.taskTimes.Store(task.GetID(), time.Now())

	if delayedQueue, ok := p.queue.(interface {
		PushAfter(context.Context, worker.Task, time.Duration) error
	}); ok {
		err := delayedQueue.PushAfter(ctx, task, delay)
		if err != nil {
			p.metrics.TasksQueued.Add(-1)
			return fmt.Errorf("failed to submit delayed task: %w", err)
		}
		return nil
	}
	return fmt.Errorf("queue does not support delayed execution")
}

// runWorker runs a worker that processes tasks from the queue
func (p *WorkerPool) runWorker(id int) {
	defer p.wg.Done()

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
			task, err := p.queue.Get(p.ctx)
			if err != nil {
				if err != queue.ErrQueueEmpty {
					if p.config.ErrorHandler != nil {
						p.config.ErrorHandler(err)
					}
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}

			if p.config.EnableMetrics {
				p.metrics.TasksInProgress.Add(1)
				p.metrics.TasksQueued.Add(-1)
				// Calculate and update queue latency
				if submitTime, ok := p.taskTimes.LoadAndDelete(task.GetID()); ok {
					latency := time.Since(submitTime.(time.Time))
					p.metrics.UpdateQueueMetrics(p.metrics.QueueSize.Load(), latency)
				}
			}

			// Execute the task with retries and save results
			p.executeTaskWithPersistence(task)
		}
	}
}

// executeTaskWithPersistence executes a task and saves the result
func (p *WorkerPool) executeTaskWithPersistence(task worker.Task) {
	startTime := time.Now()

	// Set initial task state
	task.SetStatus(worker.TaskStatusRunning)
	task.SetStartTime(startTime)
	if task.GetMetadata() == nil {
		task.SetMetadata(make(map[string]interface{}))
	}

	// Save initial state if persistence enabled
	if p.config.EnablePersistence && p.taskStore != nil {
		p.taskStore.Update(p.ctx, task)
	}

	// Execute the task with retries
	retryPolicy := p.config.DefaultRetryPolicy
	taskRetryPolicy := task.GetRetryPolicy()
	if taskRetryPolicy.MaxAttempts > 0 {
		retryPolicy = taskRetryPolicy
	}

	// Ensure at least one attempt is made
	maxAttempts := retryPolicy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	attempt := 1
	var lastErr error
	for ; attempt <= maxAttempts; attempt++ {
		// Execute the task with timeout
		timeout := p.config.DefaultTimeout
		if task.GetTimeout() > 0 {
			timeout = task.GetTimeout()
		}
		ctx, cancel := context.WithTimeout(p.ctx, timeout)
		err := task.Execute(ctx)
		cancel()

		if err == nil {
			break
		}

		lastErr = err
		if attempt < maxAttempts {
			// Calculate delay with exponential backoff
			delay := retryPolicy.InitialDelay
			if attempt > 1 {
				delay = time.Duration(float64(delay) * retryPolicy.BackoffMultiplier)
				if delay > retryPolicy.MaxDelay {
					delay = retryPolicy.MaxDelay
				}
			}
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}

	// Update final task state
	endTime := time.Now()
	task.SetEndTime(endTime)

	if p.config.EnableMetrics {
		p.metrics.TasksInProgress.Add(-1)
		if lastErr != nil {
			p.metrics.TasksFailed.Add(1)
			task.SetStatus(worker.TaskStatusFailed)
			task.SetError(lastErr)
		} else {
			p.metrics.TasksCompleted.Add(1)
			task.SetStatus(worker.TaskStatusCompleted)
		}
	}

	// Save final state if persistence enabled
	if p.config.EnablePersistence && p.taskStore != nil {
		p.taskStore.Update(p.ctx, task)
	}

	if lastErr != nil {
		if p.config.ErrorHandler != nil {
			p.config.ErrorHandler(fmt.Errorf("task failed after %d attempts: %v", maxAttempts, lastErr))
		}

		// Move permanently failed task to dead letter queue
		if dlqErr := p.deadLetterQueue.Add(p.ctx, task); dlqErr != nil {
			if p.config.ErrorHandler != nil {
				p.config.ErrorHandler(fmt.Errorf("failed to add task %s to dead letter queue: %v", task.GetID(), dlqErr))
			}
		}
	} else {
		// Remove completed task from store if persistence enabled
		if p.config.EnablePersistence && p.taskStore != nil {
			p.taskStore.Delete(p.ctx, task.GetID())
		}
	}
}

// Size returns the number of tasks in the queue
func (p *WorkerPool) Size(ctx context.Context) (int, error) {
	return p.queue.Size(ctx)
}

// Clear removes all tasks from the queue
func (p *WorkerPool) Clear(ctx context.Context) error {
	return p.queue.Clear(ctx)
}

// GetMetrics returns the current metrics
func (p *WorkerPool) GetMetrics() *metrics.Metrics {
	return p.metrics
}

// GetDeadLetterQueue returns the dead letter queue for inspection
func (p *WorkerPool) GetDeadLetterQueue() queue.Queue {
	return p.deadLetterQueue
}

// ListDeadLetterTasks returns all tasks in the dead letter queue
func (p *WorkerPool) ListDeadLetterTasks(ctx context.Context) ([]worker.Task, error) {
	if dlq, ok := p.deadLetterQueue.(*queue.DeadLetterQueue); ok {
		return dlq.List(ctx)
	}
	return nil, fmt.Errorf("dead letter queue does not support listing")
}

// RetryDeadLetterTask moves a task from dead letter queue back to main queue for retry
func (p *WorkerPool) RetryDeadLetterTask(ctx context.Context, taskID string) error {
	// Find the task first
	tasks, err := p.ListDeadLetterTasks(ctx)
	if err != nil {
		return fmt.Errorf("failed to list dead letter tasks: %w", err)
	}

	var taskToRetry worker.Task
	for _, task := range tasks {
		if task.GetID() == taskID {
			taskToRetry = task
			break
		}
	}

	if taskToRetry == nil {
		return fmt.Errorf("task not found in dead letter queue: %s", taskID)
	}

	// Remove from dead letter queue
	if err := p.deadLetterQueue.Remove(ctx, taskID); err != nil {
		return fmt.Errorf("failed to remove task from dead letter queue: %w", err)
	}

	// Add back to main queue
	return p.Submit(ctx, taskToRetry)
}

// ClearDeadLetterQueue removes all tasks from the dead letter queue
func (p *WorkerPool) ClearDeadLetterQueue(ctx context.Context) error {
	return p.deadLetterQueue.Clear(ctx)
}

// ValidateTaskDependencies checks if all dependencies for a task are satisfied
func (p *WorkerPool) ValidateTaskDependencies(ctx context.Context, task worker.Task) error {
	dependencies := task.GetDependencies()
	if len(dependencies) == 0 {
		return nil
	}

	// If persistence is not enabled, we can't validate dependencies
	if !p.config.EnablePersistence || p.taskStore == nil {
		if p.config.ErrorHandler != nil {
			p.config.ErrorHandler(fmt.Errorf("dependency validation requires persistence to be enabled for task %s", task.GetID()))
		}
		return nil // Don't fail the task, just warn
	}

	// Check each dependency
	for _, depID := range dependencies {
		// Check if dependency task exists and is completed
		depTask, err := p.taskStore.Get(ctx, depID)
		if err != nil {
			return fmt.Errorf("dependency task %s not found or not completed", depID)
		}

		if depTask.GetStatus() != worker.TaskStatusCompleted {
			return fmt.Errorf("dependency task %s is not completed (status: %s)", depID, depTask.GetStatus())
		}
	}

	return nil
}

// CheckCircularDependencies detects circular dependencies in a task
func (p *WorkerPool) CheckCircularDependencies(ctx context.Context, task worker.Task) error {
	visited := make(map[string]bool)
	recursionStack := make(map[string]bool)

	return p.checkCircularDependenciesRecursive(ctx, task.GetID(), task.GetDependencies(), visited, recursionStack)
}

func (p *WorkerPool) checkCircularDependenciesRecursive(ctx context.Context, taskID string, dependencies []string, visited, recursionStack map[string]bool) error {
	visited[taskID] = true
	recursionStack[taskID] = true

	// Check all dependencies
	for _, depID := range dependencies {
		if recursionStack[depID] {
			return fmt.Errorf("circular dependency detected: %s -> %s", taskID, depID)
		}

		if !visited[depID] {
			// If persistence is enabled, fetch the dependency task and check its dependencies
			if p.config.EnablePersistence && p.taskStore != nil {
				depTask, err := p.taskStore.Get(ctx, depID)
				if err == nil {
					// Recursively check the dependency's dependencies
					if err := p.checkCircularDependenciesRecursive(ctx, depID, depTask.GetDependencies(), visited, recursionStack); err != nil {
						return err
					}
				}
				// If task not found in store, we can't check its dependencies, but that's not a circular dependency
			}
		}
	}

	recursionStack[taskID] = false
	return nil
}

// GetTaskStore returns the task store for external access
func (p *WorkerPool) GetTaskStore() store.TaskStore {
	return p.taskStore
}

// RecoverTasks recovers tasks from the store on startup
func (p *WorkerPool) RecoverTasks(ctx context.Context) error {
	if !p.config.EnablePersistence || p.taskStore == nil {
		return nil // Nothing to recover
	}

	// Get all pending/running tasks from store
	tasks, err := p.taskStore.List(ctx, worker.TaskFilter{
		Status: worker.TaskStatusPending,
	})
	if err != nil {
		return fmt.Errorf("failed to recover tasks: %w", err)
	}

	// Re-submit recovered tasks
	for _, task := range tasks {
		if err := p.queue.Add(ctx, task); err != nil {
			if p.config.ErrorHandler != nil {
				p.config.ErrorHandler(fmt.Errorf("failed to recover task %s: %w", task.GetID(), err))
			}
		}
	}

	return nil
}
