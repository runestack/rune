package store

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

func TestMemoryTaskStore(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()

	// Test saving task
	task := &FakeTask{
		ID:        "1",
		Type:      "test",
		Priority:  1,
		Timeout:   time.Second,
		Status:    "pending",
		StartTime: time.Now(),
		EndTime:   time.Now().Add(time.Second),
	}
	err := store.Save(ctx, task)
	assert.NoError(t, err)

	// Test getting task
	retrievedTask, err := store.Get(ctx, "1")
	assert.NoError(t, err)
	assert.Equal(t, task.ID, retrievedTask.GetID())
	assert.Equal(t, task.Type, retrievedTask.GetType())
	assert.Equal(t, task.Priority, retrievedTask.GetPriority())
	assert.Equal(t, task.Timeout, retrievedTask.GetTimeout())
	assert.Equal(t, task.Status, retrievedTask.GetStatus())
	assert.Equal(t, task.StartTime, retrievedTask.GetStartTime())
	assert.Equal(t, task.EndTime, retrievedTask.GetEndTime())

	// Test getting non-existent task
	_, err = store.Get(ctx, "non-existent")
	assert.Error(t, err)

	// Test listing tasks with filter
	task2 := &FakeTask{
		ID:        "2",
		Type:      "test",
		Priority:  2,
		Timeout:   time.Second,
		Status:    "running",
		StartTime: time.Now(),
		EndTime:   time.Now().Add(time.Second),
	}
	err = store.Save(ctx, task2)
	assert.NoError(t, err)

	tasks, err := store.List(ctx, worker.TaskFilter{
		Type:   "test",
		Status: "pending",
	})
	assert.NoError(t, err)
	assert.Len(t, tasks, 1)
	assert.Equal(t, "1", tasks[0].GetID())

	tasks, err = store.List(ctx, worker.TaskFilter{
		Type:   "test",
		Status: "running",
	})
	assert.NoError(t, err)
	assert.Len(t, tasks, 1)
	assert.Equal(t, "2", tasks[0].GetID())

	// Test updating task
	task.Status = worker.TaskStatusCompleted
	err = store.Update(ctx, task)
	assert.NoError(t, err)

	retrievedTask, err = store.Get(ctx, "1")
	assert.NoError(t, err)
	assert.Equal(t, worker.TaskStatusCompleted, retrievedTask.GetStatus())

	// Test updating non-existent task
	task.ID = "non-existent"
	err = store.Update(ctx, task)
	assert.Error(t, err)

	// Test deleting task
	err = store.Delete(ctx, "1")
	assert.NoError(t, err)

	_, err = store.Get(ctx, "1")
	assert.Error(t, err)

	// Test deleting non-existent task
	err = store.Delete(ctx, "non-existent")
	assert.Error(t, err)
}

func TestPersistentTaskStore(t *testing.T) {
	ctx := context.Background()
	memoryStore := NewMemoryTaskStore()
	store := NewPersistentTaskStore(memoryStore)

	// Test saving task
	task := &FakeTask{
		ID:        "1",
		Type:      "test",
		Priority:  1,
		Timeout:   time.Second,
		Status:    "pending",
		StartTime: time.Now(),
		EndTime:   time.Now().Add(time.Second),
	}
	err := store.Save(ctx, task)
	assert.NoError(t, err)

	// Test getting task
	retrievedTask, err := store.Get(ctx, "1")
	assert.NoError(t, err)
	assert.Equal(t, task.ID, retrievedTask.GetID())
	assert.Equal(t, task.Type, retrievedTask.GetType())
	assert.Equal(t, task.Priority, retrievedTask.GetPriority())
	assert.Equal(t, task.Timeout, retrievedTask.GetTimeout())
	assert.Equal(t, task.Status, retrievedTask.GetStatus())
	assert.Equal(t, task.StartTime, retrievedTask.GetStartTime())
	assert.Equal(t, task.EndTime, retrievedTask.GetEndTime())

	// Test getting non-existent task
	_, err = store.Get(ctx, "non-existent")
	assert.Error(t, err)

	// Test listing tasks with filter
	task2 := &FakeTask{
		ID:        "2",
		Type:      "test",
		Priority:  2,
		Timeout:   time.Second,
		Status:    "running",
		StartTime: time.Now(),
		EndTime:   time.Now().Add(time.Second),
	}
	err = store.Save(ctx, task2)
	assert.NoError(t, err)

	tasks, err := store.List(ctx, worker.TaskFilter{
		Type:   "test",
		Status: "pending",
	})
	assert.NoError(t, err)
	assert.Len(t, tasks, 1)
	assert.Equal(t, "1", tasks[0].GetID())

	tasks, err = store.List(ctx, worker.TaskFilter{
		Type:   "test",
		Status: "running",
	})
	assert.NoError(t, err)
	assert.Len(t, tasks, 1)
	assert.Equal(t, "2", tasks[0].GetID())

	// Test updating task
	task.Status = worker.TaskStatusCompleted
	err = store.Update(ctx, task)
	assert.NoError(t, err)

	retrievedTask, err = store.Get(ctx, "1")
	assert.NoError(t, err)
	assert.Equal(t, worker.TaskStatusCompleted, retrievedTask.GetStatus())

	// Test updating non-existent task
	task.ID = "non-existent"
	err = store.Update(ctx, task)
	assert.Error(t, err)

	// Test deleting task
	err = store.Delete(ctx, "1")
	assert.NoError(t, err)

	_, err = store.Get(ctx, "1")
	assert.Error(t, err)

	// Test deleting non-existent task
	err = store.Delete(ctx, "non-existent")
	assert.Error(t, err)
}

func TestBadgerTaskStore(t *testing.T) {
	// Create temporary directory for test database
	tmpDir := t.TempDir()

	store, err := NewBadgerTaskStore(tmpDir)
	assert.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Test saving and retrieving a task
	task := worker.NewTask(func(ctx context.Context) error {
		return nil
	}).WithID("test-task").WithType("test").WithPriority(100)

	err = store.Save(ctx, task)
	assert.NoError(t, err)

	// Test getting the task
	retrieved, err := store.Get(ctx, "test-task")
	assert.NoError(t, err)
	assert.Equal(t, "test-task", retrieved.GetID())
	assert.Equal(t, "test", retrieved.GetType())
	assert.Equal(t, 100, retrieved.GetPriority())

	// Test getting non-existent task
	_, err = store.Get(ctx, "non-existent")
	assert.Error(t, err)
	assert.Equal(t, ErrTaskNotFound, err)

	// Test listing tasks
	tasks, err := store.List(ctx, worker.TaskFilter{})
	assert.NoError(t, err)
	assert.Len(t, tasks, 1)

	// Test updating task
	updatedTask := worker.NewTask(func(ctx context.Context) error {
		return nil
	}).WithID("test-task").WithType("test").WithPriority(200)

	err = store.Update(ctx, updatedTask)
	assert.NoError(t, err)

	retrieved, err = store.Get(ctx, "test-task")
	assert.NoError(t, err)
	assert.Equal(t, 200, retrieved.GetPriority())

	// Test deleting task
	err = store.Delete(ctx, "test-task")
	assert.NoError(t, err)

	_, err = store.Get(ctx, "test-task")
	assert.Error(t, err)
	assert.Equal(t, ErrTaskNotFound, err)
}

func TestBadgerTaskStorePersistence(t *testing.T) {
	// Create temporary directory for test database
	tmpDir := t.TempDir()

	// Create first store instance
	store1, err := NewBadgerTaskStore(tmpDir)
	assert.NoError(t, err)

	ctx := context.Background()

	// Save a task
	task := worker.NewTask(func(ctx context.Context) error {
		return nil
	}).WithID("persistent-task").WithType("test").WithPriority(100)

	err = store1.Save(ctx, task)
	assert.NoError(t, err)
	store1.Close()

	// Create second store instance with same path
	store2, err := NewBadgerTaskStore(tmpDir)
	assert.NoError(t, err)
	defer store2.Close()

	// Verify task persisted
	retrieved, err := store2.Get(ctx, "persistent-task")
	assert.NoError(t, err)
	assert.Equal(t, "persistent-task", retrieved.GetID())
	assert.Equal(t, "test", retrieved.GetType())
	assert.Equal(t, 100, retrieved.GetPriority())
}
