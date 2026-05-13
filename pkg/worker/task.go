package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TaskStatus represents the status of a task
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
	TaskStatusScheduled TaskStatus = "scheduled"
)

// RetryPolicy defines how a task should be retried
type RetryPolicy struct {
	MaxAttempts       int
	InitialDelay      time.Duration
	MaxDelay          time.Duration
	BackoffMultiplier float64
}

// TaskFilter defines filtering criteria for listing tasks
type TaskFilter struct {
	Type      string
	Status    TaskStatus
	StartTime time.Time
	EndTime   time.Time
}

// Task represents a unit of work to be executed with its execution state
type Task interface {
	// Core task properties
	GetID() string
	GetType() string
	GetPriority() int
	GetDependencies() []string

	// Execution methods
	Execute(ctx context.Context) error
	Validate() error

	// Configuration
	GetTimeout() time.Duration
	GetRetryPolicy() RetryPolicy

	// Execution state (previously in TaskResult)
	GetStatus() TaskStatus
	SetStatus(status TaskStatus)
	GetStartTime() time.Time
	SetStartTime(time time.Time)
	GetEndTime() time.Time
	SetEndTime(time time.Time)

	// Execution results (previously in TaskResult)
	GetError() error
	SetError(error error)
	GetProgress() float64
	SetProgress(progress float64)
	GetMetadata() map[string]interface{}
	SetMetadata(metadata map[string]interface{})

	// Convenience methods
	IsCompleted() bool
	IsFailed() bool
	IsRunning() bool
}

// BaseTask provides a basic implementation of the Task interface
type BaseTask struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Priority     int           `json:"priority"`
	Dependencies []string      `json:"dependencies"`
	Timeout      time.Duration `json:"timeout"`
	RetryPolicy  RetryPolicy   `json:"retry_policy"`

	// Execution state (previously in TaskResult)
	Status    TaskStatus             `json:"status"`
	StartTime time.Time              `json:"start_time"`
	EndTime   time.Time              `json:"end_time"`
	Error     error                  `json:"error"`
	Progress  float64                `json:"progress"`
	Metadata  map[string]interface{} `json:"metadata"`

	mu      sync.RWMutex                `json:"-"`
	handler func(context.Context) error `json:"-"` // Don't serialize the handler function
}

// Core task properties
func (t *BaseTask) GetID() string               { return t.ID }
func (t *BaseTask) GetType() string             { return t.Type }
func (t *BaseTask) GetPriority() int            { return t.Priority }
func (t *BaseTask) GetDependencies() []string   { return t.Dependencies }
func (t *BaseTask) GetTimeout() time.Duration   { return t.Timeout }
func (t *BaseTask) GetRetryPolicy() RetryPolicy { return t.RetryPolicy }

// Execution state
func (t *BaseTask) GetStatus() TaskStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Status
}
func (t *BaseTask) SetStatus(status TaskStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status = status
}
func (t *BaseTask) GetStartTime() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.StartTime
}
func (t *BaseTask) SetStartTime(time time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.StartTime = time
}
func (t *BaseTask) GetEndTime() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.EndTime
}
func (t *BaseTask) SetEndTime(time time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.EndTime = time
}

// Execution results
func (t *BaseTask) GetError() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Error
}
func (t *BaseTask) SetError(error error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Error = error
}
func (t *BaseTask) GetProgress() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Progress
}
func (t *BaseTask) SetProgress(progress float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Progress = progress
}
func (t *BaseTask) GetMetadata() map[string]interface{} {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Metadata
}
func (t *BaseTask) SetMetadata(metadata map[string]interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Metadata = metadata
}

// Convenience methods
func (t *BaseTask) IsCompleted() bool { return t.GetStatus() == TaskStatusCompleted }
func (t *BaseTask) IsFailed() bool    { return t.GetStatus() == TaskStatusFailed }
func (t *BaseTask) IsRunning() bool   { return t.GetStatus() == TaskStatusRunning }

func (t *BaseTask) Execute(ctx context.Context) error {
	if t.handler == nil {
		return fmt.Errorf("handler not set")
	}
	return t.handler(ctx)
}

func (t *BaseTask) Validate() error {
	return nil
}

// WithID sets a custom ID for the task
func (t *BaseTask) WithID(id string) *BaseTask {
	t.ID = id
	return t
}

// WithType sets a custom type for the task
func (t *BaseTask) WithType(typ string) *BaseTask {
	t.Type = typ
	return t
}

// WithDependencies sets the dependencies for the task
func (t *BaseTask) WithDependencies(dependencies []string) *BaseTask {
	t.Dependencies = dependencies
	return t
}

// WithPriority sets the priority for the task
func (t *BaseTask) WithPriority(priority int) *BaseTask {
	t.Priority = priority
	return t
}

// DefaultRetryPolicy returns a default retry policy
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:       3,
		InitialDelay:      time.Second,
		MaxDelay:          time.Minute,
		BackoffMultiplier: 2.0,
	}
}

// NewTask creates a new BaseTask with the provided handler function
func NewTask(handler func(context.Context) error) *BaseTask {
	return &BaseTask{
		ID:       uuid.New().String(),
		Priority: 0,
		Status:   TaskStatusPending,
		Metadata: make(map[string]interface{}),
		handler:  handler,
	}
}

// NoopTask returns a task that does nothing
func NoopTask() *BaseTask {
	return NewTask(func(ctx context.Context) error {
		return nil
	})
}
