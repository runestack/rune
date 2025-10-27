package queue

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/worker"
	"github.com/stretchr/testify/assert"
)

// FakeTask implements the worker.Task interface for testing
type FakeTask struct {
	ID        string
	Type      string
	Priority  int
	Timeout   time.Duration
	Status    worker.TaskStatus
	StartTime time.Time
	EndTime   time.Time
	Error     error
	Progress  float64
	Metadata  map[string]interface{}
}

func (t *FakeTask) GetID() string                      { return t.ID }
func (t *FakeTask) GetType() string                    { return t.Type }
func (t *FakeTask) GetPriority() int                   { return t.Priority }
func (t *FakeTask) GetDependencies() []string          { return nil }
func (t *FakeTask) Execute(ctx context.Context) error  { return nil }
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

func TestPriorityQueue(t *testing.T) {
	ctx := context.Background()
	q := NewPriorityQueue(10)

	// Test empty queue
	task, err := q.Get(ctx)
	assert.Error(t, err)
	assert.Nil(t, task)

	// Test pushing tasks
	task1 := &FakeTask{ID: "1", Priority: 1}
	task2 := &FakeTask{ID: "2", Priority: 2}
	task3 := &FakeTask{ID: "3", Priority: 3}

	err = q.Add(ctx, task1)
	assert.NoError(t, err)
	err = q.Add(ctx, task2)
	assert.NoError(t, err)
	err = q.Add(ctx, task3)
	assert.NoError(t, err)

	// Test popping tasks in priority order
	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "3", task.GetID())

	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "2", task.GetID())

	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "1", task.GetID())

	// Test queue size
	size, err := q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)

	// Test removing task
	err = q.Add(ctx, task1)
	assert.NoError(t, err)
	err = q.Remove(ctx, "1")
	assert.NoError(t, err)
	size, err = q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)

	// Test clearing queue
	err = q.Add(ctx, task1)
	assert.NoError(t, err)
	err = q.Add(ctx, task2)
	assert.NoError(t, err)
	err = q.Clear(ctx)
	assert.NoError(t, err)
	size, err = q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)
}

func TestDelayedQueue(t *testing.T) {
	ctx := context.Background()
	q := NewDelayedQueue(10)

	// Test empty queue
	task, err := q.Get(ctx)
	assert.Error(t, err)
	assert.Nil(t, task)

	// Test pushing tasks with delays
	task1 := &FakeTask{ID: "1"}
	task2 := &FakeTask{ID: "2"}
	task3 := &FakeTask{ID: "3"}

	err = q.PushAfter(ctx, task1, 100*time.Millisecond)
	assert.NoError(t, err)
	err = q.PushAfter(ctx, task2, 50*time.Millisecond)
	assert.NoError(t, err)
	err = q.PushAfter(ctx, task3, 0)
	assert.NoError(t, err)

	// Test popping tasks in delay order
	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "3", task.GetID())

	time.Sleep(60 * time.Millisecond)
	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "2", task.GetID())

	time.Sleep(50 * time.Millisecond)
	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "1", task.GetID())

	// Test queue size
	size, err := q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)

	// Test removing task
	err = q.PushAfter(ctx, task1, 100*time.Millisecond)
	assert.NoError(t, err)
	err = q.Remove(ctx, "1")
	assert.NoError(t, err)
	size, err = q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)

	// Test clearing queue
	err = q.PushAfter(ctx, task1, 100*time.Millisecond)
	assert.NoError(t, err)
	err = q.PushAfter(ctx, task2, 50*time.Millisecond)
	assert.NoError(t, err)
	err = q.Clear(ctx)
	assert.NoError(t, err)
	size, err = q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)
}

func TestRateLimitQueue(t *testing.T) {
	ctx := context.Background()
	baseQueue := NewFIFOQueue(10)
	q := NewRateLimitQueue(baseQueue, 500*time.Millisecond, 10) // 2 tasks per second

	// Test empty queue
	task, err := q.Get(ctx)
	assert.Error(t, err)
	assert.Nil(t, task)

	// Test pushing tasks
	task1 := &FakeTask{ID: "1"}
	task2 := &FakeTask{ID: "2"}
	task3 := &FakeTask{ID: "3"}

	err = q.Add(ctx, task1)
	assert.NoError(t, err)
	err = q.Add(ctx, task2)
	assert.NoError(t, err)
	err = q.Add(ctx, task3)
	assert.NoError(t, err)

	// Test popping tasks with rate limit
	start := time.Now()
	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "1", task.GetID())
	assert.True(t, time.Since(start) < 100*time.Millisecond, "First task should be available immediately")

	start = time.Now()
	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "2", task.GetID())
	assert.True(t, time.Since(start) >= 400*time.Millisecond, "Second task should be rate limited")
	assert.True(t, time.Since(start) <= 600*time.Millisecond, "Second task should not be delayed too long")

	start = time.Now()
	task, err = q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "3", task.GetID())
	assert.True(t, time.Since(start) >= 400*time.Millisecond, "Third task should be rate limited")
	assert.True(t, time.Since(start) <= 600*time.Millisecond, "Third task should not be delayed too long")

	// Test queue size
	size, err := q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)

	// Test removing task
	err = q.Add(ctx, task1)
	assert.NoError(t, err)
	err = q.Remove(ctx, "1")
	assert.NoError(t, err)
	size, err = q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)

	// Test clearing queue
	err = q.Add(ctx, task1)
	assert.NoError(t, err)
	err = q.Add(ctx, task2)
	assert.NoError(t, err)
	err = q.Clear(ctx)
	assert.NoError(t, err)
	size, err = q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)
}

func TestBatchQueue(t *testing.T) {
	ctx := context.Background()
	baseQueue := NewPriorityQueue(10)
	q := NewBatchQueue(baseQueue, 2, 10) // Batch size of 2

	// Test empty queue
	_, err := q.Get(ctx)
	assert.Error(t, err)
	assert.Equal(t, ErrBatchNotReady, err)

	_, err = q.PopBatch(ctx)
	assert.Error(t, err)
	assert.Equal(t, ErrBatchNotReady, err)

	// Test pushing tasks
	task1 := &FakeTask{ID: "1"}
	task2 := &FakeTask{ID: "2"}
	task3 := &FakeTask{ID: "3"}

	err = q.Add(ctx, task1)
	assert.NoError(t, err)

	// With only one task, we shouldn't be able to get a batch
	_, err = q.PopBatch(ctx)
	assert.Error(t, err)
	assert.Equal(t, ErrBatchNotReady, err)

	// Add a second task to reach batch size
	err = q.Add(ctx, task2)
	assert.NoError(t, err)

	// Now we should be able to get a batch
	batch, err := q.PopBatch(ctx)
	assert.NoError(t, err)
	assert.Len(t, batch, 2)
	assert.Equal(t, "1", batch[0].GetID())
	assert.Equal(t, "2", batch[1].GetID())

	// Add task3 and verify Get returns ErrBatchNotReady since it's just one task
	err = q.Add(ctx, task3)
	assert.NoError(t, err)

	// Verify size
	size, err := q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 1, size)

	// Can't get individual tasks if less than batch size
	_, err = q.Get(ctx)
	assert.Error(t, err)
	assert.Equal(t, ErrBatchNotReady, err)

	// Add another task to make a full batch
	task4 := &FakeTask{ID: "4"}
	err = q.Add(ctx, task4)
	assert.NoError(t, err)

	// Now should be able to get a task via Get
	task, err := q.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "3", task.GetID())

	// Queue should have one task left
	size, err = q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 1, size)

	// Test removing task
	err = q.Add(ctx, task1)
	assert.NoError(t, err)
	err = q.Remove(ctx, "1")
	assert.NoError(t, err)

	// Test clearing queue
	err = q.Add(ctx, task1)
	assert.NoError(t, err)
	err = q.Add(ctx, task2)
	assert.NoError(t, err)
	err = q.Clear(ctx)
	assert.NoError(t, err)
	size, err = q.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)
}

func TestDeadLetterQueue(t *testing.T) {
	ctx := context.Background()
	queue := NewDeadLetterQueue(3)

	// Test empty queue
	_, err := queue.Get(ctx)
	assert.Error(t, err)
	assert.Equal(t, ErrQueueEmpty, err)

	_, err = queue.Peek(ctx)
	assert.Error(t, err)
	assert.Equal(t, ErrQueueEmpty, err)

	size, err := queue.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)

	// Test adding tasks
	task1 := &FakeTask{ID: "task1", Priority: 1}
	task2 := &FakeTask{ID: "task2", Priority: 2}
	task3 := &FakeTask{ID: "task3", Priority: 3}

	err = queue.Add(ctx, task1)
	assert.NoError(t, err)

	err = queue.Add(ctx, task2)
	assert.NoError(t, err)

	err = queue.Add(ctx, task3)
	assert.NoError(t, err)

	// Test capacity limit
	task4 := &FakeTask{ID: "task4", Priority: 4}
	err = queue.Add(ctx, task4)
	assert.Error(t, err)
	assert.Equal(t, ErrQueueFull, err)

	// Test size
	size, err = queue.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 3, size)

	// Test peek (should return first task without removing)
	peeked, err := queue.Peek(ctx)
	assert.NoError(t, err)
	assert.Equal(t, task1, peeked)

	size, err = queue.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 3, size) // Size should remain the same

	// Test FIFO behavior
	retrieved, err := queue.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, task1, retrieved)

	retrieved, err = queue.Get(ctx)
	assert.NoError(t, err)
	assert.Equal(t, task2, retrieved)

	// Test list functionality
	tasks, err := queue.List(ctx)
	assert.NoError(t, err)
	assert.Len(t, tasks, 1)
	assert.Equal(t, task3, tasks[0])

	// Test remove
	err = queue.Remove(ctx, "task3")
	assert.NoError(t, err)

	size, err = queue.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)

	// Test remove non-existent task
	err = queue.Remove(ctx, "non-existent")
	assert.Error(t, err)

	// Test clear
	err = queue.Add(ctx, task1)
	assert.NoError(t, err)
	err = queue.Add(ctx, task2)
	assert.NoError(t, err)

	err = queue.Clear(ctx)
	assert.NoError(t, err)

	size, err = queue.Size(ctx)
	assert.NoError(t, err)
	assert.Equal(t, 0, size)
}
