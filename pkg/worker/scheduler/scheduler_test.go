package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/worker"
	"github.com/runestack/rune/pkg/worker/pool"
	"github.com/runestack/rune/pkg/worker/queue"
	"github.com/stretchr/testify/assert"
)

// FakeTask implements the worker.Task interface for testing
type FakeTask struct {
	ID             string
	Type           string
	Priority       int
	Timeout        time.Duration
	ExecuteFn      func(ctx context.Context) error
	executionCount int
	Status         worker.TaskStatus
	StartTime      time.Time
	EndTime        time.Time
	Error          error
	Progress       float64
	Metadata       map[string]interface{}
}

func (t *FakeTask) GetID() string             { return t.ID }
func (t *FakeTask) GetType() string           { return t.Type }
func (t *FakeTask) GetPriority() int          { return t.Priority }
func (t *FakeTask) GetDependencies() []string { return nil }
func (t *FakeTask) Execute(ctx context.Context) error {
	t.executionCount++
	if t.ExecuteFn != nil {
		return t.ExecuteFn(ctx)
	}
	return nil
}
func (t *FakeTask) Validate() error                    { return nil }
func (t *FakeTask) GetTimeout() time.Duration          { return t.Timeout }
func (t *FakeTask) GetRetryPolicy() worker.RetryPolicy { return worker.RetryPolicy{} }

// Execution state
func (t *FakeTask) GetStatus() worker.TaskStatus       { return t.Status }
func (t *FakeTask) SetStatus(status worker.TaskStatus) { t.Status = status }
func (t *FakeTask) GetStartTime() time.Time            { return t.StartTime }
func (t *FakeTask) SetStartTime(time time.Time)        { t.StartTime = time }
func (t *FakeTask) GetEndTime() time.Time              { return t.EndTime }
func (t *FakeTask) SetEndTime(time time.Time)          { t.EndTime = time }

// Execution results
func (t *FakeTask) GetError() error                             { return t.Error }
func (t *FakeTask) SetError(error error)                        { t.Error = error }
func (t *FakeTask) GetProgress() float64                        { return t.Progress }
func (t *FakeTask) SetProgress(progress float64)                { t.Progress = progress }
func (t *FakeTask) GetMetadata() map[string]interface{}         { return t.Metadata }
func (t *FakeTask) SetMetadata(metadata map[string]interface{}) { t.Metadata = metadata }

// Convenience methods
func (t *FakeTask) IsCompleted() bool { return t.Status == worker.TaskStatusCompleted }
func (t *FakeTask) IsFailed() bool    { return t.Status == worker.TaskStatusFailed }
func (t *FakeTask) IsRunning() bool   { return t.Status == worker.TaskStatusRunning }

func TestScheduler(t *testing.T) {
	ctx := context.Background()

	// Test creating scheduler with invalid worker pool config
	_, err := NewScheduler(SchedulerConfig{
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:    0,
			QueueType:     queue.QueueTypePriority,
			QueueCapacity: 10,
		},
	})
	assert.Error(t, err)

	// Test creating valid scheduler
	scheduler, err := NewScheduler(SchedulerConfig{
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:     1,
			QueueType:      queue.QueueTypePriority,
			QueueCapacity:  10,
			DefaultTimeout: time.Second,
		},
		DefaultInterval: time.Second,
		MaxTasks:        10,
	})
	assert.NoError(t, err)

	// Test scheduling task
	task := &FakeTask{
		ID: "1",
		ExecuteFn: func(ctx context.Context) error {
			return nil
		},
	}
	err = scheduler.ScheduleInterval(ctx, task, time.Second)
	assert.NoError(t, err)

	// Test scheduling task with default interval
	task = &FakeTask{
		ID: "2",
		ExecuteFn: func(ctx context.Context) error {
			return nil
		},
	}
	err = scheduler.ScheduleInterval(ctx, task, 0)
	assert.NoError(t, err)

	// Test scheduling task when max tasks reached
	for i := 0; i < 8; i++ {
		task = &FakeTask{
			ID: string(rune('3' + i)),
			ExecuteFn: func(ctx context.Context) error {
				return nil
			},
		}
		err = scheduler.ScheduleInterval(ctx, task, time.Second)
		assert.NoError(t, err)
	}

	task = &FakeTask{
		ID: "11",
		ExecuteFn: func(ctx context.Context) error {
			return nil
		},
	}
	err = scheduler.ScheduleInterval(ctx, task, time.Second)
	assert.Error(t, err)

	// Test listing scheduled tasks
	tasks := scheduler.List(ctx)
	assert.Len(t, tasks, 10)

	// Test unscheduling task
	err = scheduler.Unschedule(ctx, "1")
	assert.NoError(t, err)
	tasks = scheduler.List(ctx)
	assert.Len(t, tasks, 9)

	// Test unscheduling non-existent task
	err = scheduler.Unschedule(ctx, "non-existent")
	assert.Error(t, err)

	// Test starting and stopping scheduler
	scheduler.Start()
	time.Sleep(2 * time.Second)
	scheduler.Stop()
}

func TestSchedulerWithTaskExecution(t *testing.T) {
	ctx := context.Background()

	scheduler, err := NewScheduler(SchedulerConfig{
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:     1,
			QueueType:      queue.QueueTypePriority,
			QueueCapacity:  10,
			DefaultTimeout: time.Second,
		},
		DefaultInterval: 100 * time.Millisecond,
		MaxTasks:        10,
	})
	assert.NoError(t, err)

	// Test task execution
	executionCount := 0
	task := &FakeTask{
		ID: "1",
		ExecuteFn: func(ctx context.Context) error {
			executionCount++
			return nil
		},
	}
	err = scheduler.ScheduleInterval(ctx, task, 100*time.Millisecond)
	assert.NoError(t, err)

	scheduler.Start()
	time.Sleep(700 * time.Millisecond)
	scheduler.Stop()

	assert.GreaterOrEqual(t, executionCount, 4)
}

func TestSchedulerWithTaskError(t *testing.T) {
	ctx := context.Background()

	scheduler, err := NewScheduler(SchedulerConfig{
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:     1,
			QueueType:      queue.QueueTypePriority,
			QueueCapacity:  10,
			DefaultTimeout: time.Second,
		},
		DefaultInterval: 100 * time.Millisecond,
		MaxTasks:        10,
	})
	assert.NoError(t, err)

	// Test task with error
	executionCount := 0
	task := &FakeTask{
		ID: "1",
		ExecuteFn: func(ctx context.Context) error {
			executionCount++
			return assert.AnError
		},
	}
	err = scheduler.ScheduleInterval(ctx, task, 100*time.Millisecond)
	assert.NoError(t, err)

	scheduler.Start()
	time.Sleep(700 * time.Millisecond)
	scheduler.Stop()

	assert.GreaterOrEqual(t, executionCount, 4)
}

func TestSchedulerWithCron(t *testing.T) {
	config := SchedulerConfig{
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:     1,
			QueueType:      queue.QueueTypePriority,
			QueueCapacity:  10,
			DefaultTimeout: time.Second,
			EnableMetrics:  true,
		},
		MaxTasks: 10,
	}

	scheduler, err := NewScheduler(config)
	assert.NoError(t, err)
	assert.NotNil(t, scheduler)

	// Start the scheduler
	scheduler.Start()

	// Create a task that will be executed by cron
	task := worker.NewTask(func(ctx context.Context) error {
		return nil
	})

	// Schedule the task to run every second
	err = scheduler.ScheduleCron(task, "@every 1s", 2)
	assert.NoError(t, err)

	// Wait for a few executions
	time.Sleep(3 * time.Second)

	// Check that the task was executed
	tasks := scheduler.List(context.Background())
	assert.Len(t, tasks, 1)
	assert.Equal(t, task.GetID(), tasks[0].Task.GetID())
	assert.Equal(t, "@every 1s", tasks[0].CronSchedule)

	// Unschedule the task
	err = scheduler.Unschedule(context.Background(), task.GetID())
	assert.NoError(t, err)

	// Verify task was removed
	tasks = scheduler.List(context.Background())
	assert.Len(t, tasks, 0)

	// Stop the scheduler
	scheduler.Stop()
}

func TestSchedulerWithInvalidCron(t *testing.T) {
	config := SchedulerConfig{
		MaxTasks: 10,
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:     2,
			QueueType:      queue.QueueTypePriority,
			QueueCapacity:  10,
			DefaultTimeout: time.Second,
		},
	}

	scheduler, err := NewScheduler(config)
	assert.NoError(t, err)
	assert.NotNil(t, scheduler)

	// Start the scheduler
	scheduler.Start()
	defer scheduler.Stop()

	// Test case: Task with invalid cron expression
	task := worker.NewTask(func(ctx context.Context) error {
		return nil
	})

	// Try to schedule task with invalid cron expression
	err = scheduler.ScheduleCron(task, "invalid", 0)
	assert.Error(t, err)
}

func TestSchedulerWithMixedScheduling(t *testing.T) {
	config := SchedulerConfig{
		MaxTasks: 10,
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:     2,
			QueueType:      queue.QueueTypePriority,
			QueueCapacity:  10,
			DefaultTimeout: time.Second,
		},
	}

	scheduler, err := NewScheduler(config)
	assert.NoError(t, err)
	assert.NotNil(t, scheduler)

	// Start the scheduler
	scheduler.Start()
	defer scheduler.Stop()

	// Test case 1: Interval-based task (use atomic for thread-safe counter)
	var intervalCount int64
	intervalTask := worker.NewTask(func(ctx context.Context) error {
		atomic.AddInt64(&intervalCount, 1)
		return nil
	})

	// Schedule interval task to run every 100ms
	err = scheduler.Schedule(intervalTask, 100*time.Millisecond, 0)
	assert.NoError(t, err)

	// Test case 2: Cron-based task (use atomic for thread-safe counter)
	var cronCount int64
	cronTask := worker.NewTask(func(ctx context.Context) error {
		atomic.AddInt64(&cronCount, 1)
		return nil
	})

	// Schedule cron task to run every second
	err = scheduler.ScheduleCron(cronTask, "@every 1s", 0)
	assert.NoError(t, err)

	// Wait for tasks to run
	time.Sleep(2 * time.Second)

	// Verify both tasks were executed
	assert.Greater(t, atomic.LoadInt64(&intervalCount), int64(0))
	assert.Greater(t, atomic.LoadInt64(&cronCount), int64(0))
}

func TestSchedulerWithMaxRuns(t *testing.T) {
	config := SchedulerConfig{
		MaxTasks: 10,
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:     2,
			QueueType:      queue.QueueTypePriority,
			QueueCapacity:  10,
			DefaultTimeout: time.Second,
		},
	}

	scheduler, err := NewScheduler(config)
	assert.NoError(t, err)
	assert.NotNil(t, scheduler)

	// Start the scheduler
	scheduler.Start()
	defer scheduler.Stop()

	// Test case 1: Interval-based task with max runs (use atomic for thread-safe counter)
	var executionCount1 int64
	task1 := worker.NewTask(func(ctx context.Context) error {
		atomic.AddInt64(&executionCount1, 1)
		return nil
	})

	// Schedule task to run every 100ms, max 3 times
	err = scheduler.Schedule(task1, 100*time.Millisecond, 3)
	assert.NoError(t, err)

	// Wait for task to complete its runs
	time.Sleep(500 * time.Millisecond)

	// Verify task was executed exactly 3 times
	assert.Equal(t, int64(3), atomic.LoadInt64(&executionCount1))

	// Test case 2: Cron-based task with max runs (use atomic for thread-safe counter)
	var executionCount2 int64
	task2 := worker.NewTask(func(ctx context.Context) error {
		atomic.AddInt64(&executionCount2, 1)
		return nil
	})

	// Schedule task to run every second, max 2 times
	err = scheduler.ScheduleCron(task2, "@every 1s", 2)
	assert.NoError(t, err)

	// Wait for task to complete its runs
	time.Sleep(3 * time.Second)

	// Verify task was executed exactly 2 times
	assert.Equal(t, int64(2), atomic.LoadInt64(&executionCount2))
}

func TestSchedulerWithZeroMaxRuns(t *testing.T) {
	config := SchedulerConfig{
		MaxTasks: 10,
		WorkerPoolConfig: pool.WorkerPoolConfig{
			NumWorkers:     2,
			QueueType:      queue.QueueTypePriority,
			QueueCapacity:  10,
			DefaultTimeout: time.Second,
		},
	}

	scheduler, err := NewScheduler(config)
	assert.NoError(t, err)
	assert.NotNil(t, scheduler)

	// Start the scheduler
	scheduler.Start()
	defer scheduler.Stop()

	// Test case: Task with zero max runs (should run indefinitely) (use atomic for thread-safe counter)
	var executionCount int64
	task := worker.NewTask(func(ctx context.Context) error {
		atomic.AddInt64(&executionCount, 1)
		return nil
	})

	// Schedule task to run every 100ms, max 0 times (indefinite)
	err = scheduler.Schedule(task, 100*time.Millisecond, 0)
	assert.NoError(t, err)

	// Wait for task to run multiple times
	time.Sleep(800 * time.Millisecond)

	// Verify task was executed multiple times
	assert.GreaterOrEqual(t, atomic.LoadInt64(&executionCount), int64(5))
}
