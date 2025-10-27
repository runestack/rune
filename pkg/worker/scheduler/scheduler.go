package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/runestack/rune/pkg/worker"
	"github.com/runestack/rune/pkg/worker/pool"
	"github.com/runestack/rune/pkg/worker/store"
)

// SchedulerConfig defines the configuration for a scheduler
type SchedulerConfig struct {
	// Worker pool configuration
	WorkerPoolConfig pool.WorkerPoolConfig

	// Default schedule interval
	DefaultInterval time.Duration

	// Maximum number of scheduled tasks
	MaxTasks int

	// Task store for persistence
	TaskStore store.TaskStore

	// Enable persistence for scheduled tasks
	EnablePersistence bool
}

// Scheduler manages scheduled tasks
type Scheduler struct {
	config    SchedulerConfig
	pool      *pool.WorkerPool
	tasks     map[string]*ScheduledTask
	mu        sync.RWMutex
	ctx       context.Context
	cancel    context.CancelFunc
	cron      *cron.Cron
	taskStore store.TaskStore
}

// ScheduledTask represents a task with scheduling information
type ScheduledTask struct {
	Task         worker.Task
	Interval     time.Duration
	NextRun      time.Time
	LastRun      time.Time
	IsActive     bool
	CronSchedule string
	CronEntryID  cron.EntryID
	MaxRuns      int // Maximum number of times the task should run
	CurrentRuns  int // Current number of times the task has run
}

// NewScheduler creates a new scheduler with the given configuration
func NewScheduler(config SchedulerConfig) (*Scheduler, error) {
	// Update worker pool config to enable persistence if scheduler persistence is enabled
	if config.EnablePersistence {
		config.WorkerPoolConfig.EnablePersistence = true
		if config.WorkerPoolConfig.TaskStore == nil && config.TaskStore != nil {
			config.WorkerPoolConfig.TaskStore = config.TaskStore
		}
	}

	pool, err := pool.NewWorkerPool(config.WorkerPoolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create worker pool: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Use standard parser that supports 5-field cron expressions (minute hour day month weekday)
	c := cron.New(cron.WithParser(cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)))

	// Set up task store
	var taskStore store.TaskStore
	if config.EnablePersistence {
		if config.TaskStore == nil {
			taskStore = store.NewMemoryTaskStore()
		} else {
			taskStore = config.TaskStore
		}
	}

	scheduler := &Scheduler{
		config:    config,
		pool:      pool,
		tasks:     make(map[string]*ScheduledTask),
		ctx:       ctx,
		cancel:    cancel,
		cron:      c,
		taskStore: taskStore,
	}

	return scheduler, nil
}

// Start starts the scheduler
func (s *Scheduler) Start() {
	s.pool.Start()
	s.cron.Start()

	// Recover scheduled tasks if persistence is enabled
	if s.config.EnablePersistence {
		if err := s.recoverScheduledTasks(); err != nil {
			// Log error but don't fail startup
			if s.config.WorkerPoolConfig.ErrorHandler != nil {
				s.config.WorkerPoolConfig.ErrorHandler(fmt.Errorf("failed to recover scheduled tasks: %w", err))
			}
		}
	}

	go s.run()
}

// Stop stops the scheduler
func (s *Scheduler) Stop() {
	s.cancel()
	s.cron.Stop()
	s.pool.Stop()
}

// ScheduleInterval schedules a task to run at the specified interval
func (s *Scheduler) ScheduleInterval(ctx context.Context, task worker.Task, interval time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tasks) >= s.config.MaxTasks {
		return fmt.Errorf("maximum number of tasks reached")
	}

	if interval == 0 {
		interval = s.config.DefaultInterval
	}

	scheduledTask := &ScheduledTask{
		Task:     task,
		Interval: interval,
		NextRun:  time.Now().Add(interval),
		IsActive: true,
	}

	s.tasks[task.GetID()] = scheduledTask

	// Save to persistent store if enabled
	if s.config.EnablePersistence && s.taskStore != nil {
		if err := s.saveScheduledTask(ctx, scheduledTask); err != nil {
			// Remove from memory if persistence failed
			delete(s.tasks, task.GetID())
			return fmt.Errorf("failed to save scheduled task: %w", err)
		}
	}

	return nil
}

// ScheduleCron schedules a task to run based on a cron expression
func (s *Scheduler) ScheduleCron(task worker.Task, cronExpr string, maxRuns int) error {
	if s.cron == nil {
		return fmt.Errorf("cron scheduler not initialized")
	}

	entryID, err := s.cron.AddFunc(cronExpr, func() {
		// Check if we've reached max runs
		s.mu.Lock()
		scheduledTask, exists := s.tasks[task.GetID()]
		if !exists {
			s.mu.Unlock()
			return
		}

		if scheduledTask.MaxRuns > 0 && scheduledTask.CurrentRuns >= scheduledTask.MaxRuns {
			// Remove the task from cron scheduler
			s.cron.Remove(scheduledTask.CronEntryID)
			scheduledTask.IsActive = false
			s.tasks[task.GetID()] = scheduledTask

			// Update in persistent store
			if s.config.EnablePersistence && s.taskStore != nil {
				s.saveScheduledTask(s.ctx, scheduledTask)
			}

			s.mu.Unlock()
			return
		}

		// Submit the task to the worker pool
		err := s.pool.Submit(s.ctx, task)
		if err != nil {
			if s.config.WorkerPoolConfig.ErrorHandler != nil {
				s.config.WorkerPoolConfig.ErrorHandler(fmt.Errorf("failed to submit cron task %s: %w", task.GetID(), err))
			}
			s.mu.Unlock()
			return
		}

		// Update task state
		scheduledTask.LastRun = time.Now()
		scheduledTask.CurrentRuns++
		s.tasks[task.GetID()] = scheduledTask

		// Update in persistent store
		if s.config.EnablePersistence && s.taskStore != nil {
			s.saveScheduledTask(s.ctx, scheduledTask)
		}

		s.mu.Unlock()
	})

	if err != nil {
		return fmt.Errorf("failed to schedule cron task: %w", err)
	}

	scheduledTask := ScheduledTask{
		Task:         task,
		CronSchedule: cronExpr,
		CronEntryID:  entryID,
		IsActive:     true,
		MaxRuns:      maxRuns,
		CurrentRuns:  0,
	}

	s.mu.Lock()
	s.tasks[task.GetID()] = &scheduledTask
	s.mu.Unlock()

	// Save to persistent store if enabled
	if s.config.EnablePersistence && s.taskStore != nil {
		if err := s.saveScheduledTask(context.Background(), &scheduledTask); err != nil {
			// Remove from memory and cron if persistence failed
			s.mu.Lock()
			delete(s.tasks, task.GetID())
			s.mu.Unlock()
			s.cron.Remove(entryID)
			return fmt.Errorf("failed to save scheduled task: %w", err)
		}
	}

	return nil
}

// Unschedule removes a scheduled task
func (s *Scheduler) Unschedule(ctx context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, exists := s.tasks[taskID]
	if !exists {
		return fmt.Errorf("task not found: %s", taskID)
	}

	// If it's a cron task, remove it from the cron scheduler
	if task.CronSchedule != "" {
		s.cron.Remove(task.CronEntryID)
	}

	delete(s.tasks, taskID)

	// Remove from persistent store if enabled
	if s.config.EnablePersistence && s.taskStore != nil {
		if err := s.deleteScheduledTask(ctx, taskID); err != nil {
			// Log error but don't fail the operation
			if s.config.WorkerPoolConfig.ErrorHandler != nil {
				s.config.WorkerPoolConfig.ErrorHandler(fmt.Errorf("failed to delete scheduled task from store: %w", err))
			}
		}
	}

	return nil
}

// List returns all scheduled tasks
func (s *Scheduler) List(ctx context.Context) []*ScheduledTask {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tasks := make([]*ScheduledTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		tasks = append(tasks, task)
	}

	return tasks
}

// run runs the scheduler loop
func (s *Scheduler) run() {
	ticker := time.NewTicker(50 * time.Millisecond) // Run every 50ms to handle sub-second intervals
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.processTasks()
		}
	}
}

// processTasks processes tasks that are due to run
func (s *Scheduler) processTasks() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, task := range s.tasks {
		if !task.IsActive {
			continue
		}

		// Skip cron-scheduled tasks as they are managed by the cron scheduler
		if task.CronSchedule != "" {
			continue
		}

		// Check if it's time to run the task
		if now.After(task.NextRun) {
			// Check if we've reached max runs
			if task.MaxRuns > 0 && task.CurrentRuns >= task.MaxRuns {
				task.IsActive = false
				s.tasks[task.Task.GetID()] = task

				// Update in persistent store
				if s.config.EnablePersistence && s.taskStore != nil {
					s.saveScheduledTask(s.ctx, task)
				}

				continue
			}

			// Submit the task to the worker pool
			if err := s.pool.Submit(s.ctx, task.Task); err != nil {
				// Log the error and update next run time
				if s.config.WorkerPoolConfig.ErrorHandler != nil {
					s.config.WorkerPoolConfig.ErrorHandler(fmt.Errorf("failed to submit task %s: %w", task.Task.GetID(), err))
				}
				// Update next run time to retry after a short delay
				task.NextRun = now.Add(time.Second)
				s.tasks[task.Task.GetID()] = task

				// Update in persistent store
				if s.config.EnablePersistence && s.taskStore != nil {
					s.saveScheduledTask(s.ctx, task)
				}

				continue
			}

			// Update task state
			task.LastRun = now
			task.NextRun = now.Add(task.Interval)
			task.CurrentRuns++
			s.tasks[task.Task.GetID()] = task

			// Update in persistent store
			if s.config.EnablePersistence && s.taskStore != nil {
				s.saveScheduledTask(s.ctx, task)
			}
		}
	}
}

// Schedule schedules a task to run at the specified interval
func (s *Scheduler) Schedule(task worker.Task, interval time.Duration, maxRuns int) error {
	if interval <= 0 {
		return fmt.Errorf("interval must be greater than 0")
	}

	scheduledTask := ScheduledTask{
		Task:        task,
		Interval:    interval,
		NextRun:     time.Now().Add(interval),
		IsActive:    true,
		MaxRuns:     maxRuns,
		CurrentRuns: 0,
	}

	s.mu.Lock()
	s.tasks[task.GetID()] = &scheduledTask
	s.mu.Unlock()

	// Save to persistent store if enabled
	if s.config.EnablePersistence && s.taskStore != nil {
		if err := s.saveScheduledTask(context.Background(), &scheduledTask); err != nil {
			// Remove from memory if persistence failed
			s.mu.Lock()
			delete(s.tasks, task.GetID())
			s.mu.Unlock()
			return fmt.Errorf("failed to save scheduled task: %w", err)
		}
	}

	return nil
}

// saveScheduledTask saves a scheduled task to the persistent store
func (s *Scheduler) saveScheduledTask(ctx context.Context, scheduledTask *ScheduledTask) error {
	if s.taskStore == nil {
		return nil
	}

	// Create a wrapper task that includes scheduling metadata
	taskWithSchedule := &scheduledTaskWrapper{
		Task:         scheduledTask.Task,
		Interval:     scheduledTask.Interval,
		NextRun:      scheduledTask.NextRun,
		LastRun:      scheduledTask.LastRun,
		IsActive:     scheduledTask.IsActive,
		CronSchedule: scheduledTask.CronSchedule,
		MaxRuns:      scheduledTask.MaxRuns,
		CurrentRuns:  scheduledTask.CurrentRuns,
	}

	return s.taskStore.Save(ctx, taskWithSchedule)
}

// deleteScheduledTask removes a scheduled task from the persistent store
func (s *Scheduler) deleteScheduledTask(ctx context.Context, taskID string) error {
	if s.taskStore == nil {
		return nil
	}
	return s.taskStore.Delete(ctx, taskID)
}

// recoverScheduledTasks recovers scheduled tasks from the persistent store
func (s *Scheduler) recoverScheduledTasks() error {
	if s.taskStore == nil {
		return nil
	}

	// Get all scheduled tasks from store
	tasks, err := s.taskStore.List(s.ctx, worker.TaskFilter{
		Type: "scheduled", // We'll use this type to identify scheduled tasks
	})
	if err != nil {
		return fmt.Errorf("failed to list scheduled tasks: %w", err)
	}

	// Restore scheduled tasks
	for _, task := range tasks {
		if wrapper, ok := task.(*scheduledTaskWrapper); ok {
			scheduledTask := &ScheduledTask{
				Task:         wrapper.Task,
				Interval:     wrapper.Interval,
				NextRun:      wrapper.NextRun,
				LastRun:      wrapper.LastRun,
				IsActive:     wrapper.IsActive,
				CronSchedule: wrapper.CronSchedule,
				MaxRuns:      wrapper.MaxRuns,
				CurrentRuns:  wrapper.CurrentRuns,
			}

			// Re-register cron tasks
			if wrapper.CronSchedule != "" && wrapper.IsActive {
				entryID, err := s.cron.AddFunc(wrapper.CronSchedule, func() {
					// Same logic as in ScheduleCron
					s.mu.Lock()
					defer s.mu.Unlock()

					if scheduledTask.MaxRuns > 0 && scheduledTask.CurrentRuns >= scheduledTask.MaxRuns {
						s.cron.Remove(scheduledTask.CronEntryID)
						scheduledTask.IsActive = false
						s.tasks[scheduledTask.Task.GetID()] = scheduledTask
						s.saveScheduledTask(s.ctx, scheduledTask)
						return
					}

					if err := s.pool.Submit(s.ctx, scheduledTask.Task); err != nil {
						if s.config.WorkerPoolConfig.ErrorHandler != nil {
							s.config.WorkerPoolConfig.ErrorHandler(fmt.Errorf("failed to submit recovered cron task %s: %w", scheduledTask.Task.GetID(), err))
						}
						return
					}

					scheduledTask.LastRun = time.Now()
					scheduledTask.CurrentRuns++
					s.tasks[scheduledTask.Task.GetID()] = scheduledTask
					s.saveScheduledTask(s.ctx, scheduledTask)
				})

				if err != nil {
					if s.config.WorkerPoolConfig.ErrorHandler != nil {
						s.config.WorkerPoolConfig.ErrorHandler(fmt.Errorf("failed to recover cron task %s: %w", wrapper.Task.GetID(), err))
					}
					continue
				}

				scheduledTask.CronEntryID = entryID
			}

			s.mu.Lock()
			s.tasks[wrapper.Task.GetID()] = scheduledTask
			s.mu.Unlock()
		}
	}

	return nil
}

// scheduledTaskWrapper wraps a task with scheduling metadata for persistence
type scheduledTaskWrapper struct {
	worker.Task
	Interval     time.Duration `json:"interval"`
	NextRun      time.Time     `json:"next_run"`
	LastRun      time.Time     `json:"last_run"`
	IsActive     bool          `json:"is_active"`
	CronSchedule string        `json:"cron_schedule"`
	MaxRuns      int           `json:"max_runs"`
	CurrentRuns  int           `json:"current_runs"`
}

// Override GetType to identify scheduled tasks
func (w *scheduledTaskWrapper) GetType() string {
	return "scheduled"
}

// GetTaskStore returns the task store for external access
func (s *Scheduler) GetTaskStore() store.TaskStore {
	return s.taskStore
}
