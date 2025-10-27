package finalizers

import (
	"context"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// FinalizerInterface defines the contract for finalizers
type FinalizerInterface interface {
	// GetID returns the unique identifier for this finalizer
	GetID() string

	// GetType returns the type of this finalizer
	GetType() types.FinalizerType

	// GetStatus returns the current status of this finalizer
	GetStatus() types.FinalizerStatus

	// SetStatus sets the status of this finalizer
	SetStatus(status types.FinalizerStatus)

	// GetError returns any error from the last execution
	GetError() string

	// SetError sets an error message
	SetError(err string)

	// GetDependencies returns the list of finalizers this one depends on
	GetDependencies() []types.FinalizerDependency

	// Execute performs the cleanup operation
	Execute(ctx context.Context, service *types.Service) error

	// Validate checks if the finalizer can be executed
	Validate(service *types.Service) error

	// GetCreatedAt returns when this finalizer was created
	GetCreatedAt() time.Time

	// GetUpdatedAt returns when this finalizer was last updated
	GetUpdatedAt() time.Time

	// GetCompletedAt returns when this finalizer completed (if it has)
	GetCompletedAt() *time.Time

	// SetCompletedAt sets the completion time
	SetCompletedAt(t time.Time)

	// ToFinalizer converts to the types.Finalizer struct
	ToFinalizer() types.Finalizer
}

// BaseFinalizer provides a basic implementation of FinalizerInterface
type BaseFinalizer struct {
	ID           string                      `json:"id"`
	Type         types.FinalizerType         `json:"type"`
	Status       types.FinalizerStatus       `json:"status"`
	Error        string                      `json:"error,omitempty"`
	Dependencies []types.FinalizerDependency `json:"dependencies,omitempty"`
	CreatedAt    time.Time                   `json:"created_at"`
	UpdatedAt    time.Time                   `json:"updated_at"`
	CompletedAt  *time.Time                  `json:"completed_at,omitempty"`
}

// GetID returns the unique identifier for this finalizer
func (f *BaseFinalizer) GetID() string {
	return f.ID
}

// GetType returns the type of this finalizer
func (f *BaseFinalizer) GetType() types.FinalizerType {
	return f.Type
}

// GetStatus returns the current status of this finalizer
func (f *BaseFinalizer) GetStatus() types.FinalizerStatus {
	return f.Status
}

// SetStatus sets the status of this finalizer
func (f *BaseFinalizer) SetStatus(status types.FinalizerStatus) {
	f.Status = status
	f.UpdatedAt = time.Now()
}

// GetError returns any error from the last execution
func (f *BaseFinalizer) GetError() string {
	return f.Error
}

// SetError sets an error message
func (f *BaseFinalizer) SetError(err string) {
	f.Error = err
	f.UpdatedAt = time.Now()
}

// GetDependencies returns the list of finalizers this one depends on
func (f *BaseFinalizer) GetDependencies() []types.FinalizerDependency {
	return f.Dependencies
}

// GetCreatedAt returns when this finalizer was created
func (f *BaseFinalizer) GetCreatedAt() time.Time {
	return f.CreatedAt
}

// GetUpdatedAt returns when this finalizer was last updated
func (f *BaseFinalizer) GetUpdatedAt() time.Time {
	return f.UpdatedAt
}

// GetCompletedAt returns when this finalizer completed (if it has)
func (f *BaseFinalizer) GetCompletedAt() *time.Time {
	return f.CompletedAt
}

// SetCompletedAt sets the completion time
func (f *BaseFinalizer) SetCompletedAt(t time.Time) {
	f.CompletedAt = &t
	f.UpdatedAt = time.Now()
}

// ToFinalizer converts to the types.Finalizer struct
func (f *BaseFinalizer) ToFinalizer() types.Finalizer {
	return types.Finalizer{
		ID:           f.ID,
		Type:         f.Type,
		Status:       f.Status,
		Error:        f.Error,
		Dependencies: f.Dependencies,
		CreatedAt:    f.CreatedAt,
		UpdatedAt:    f.UpdatedAt,
		CompletedAt:  f.CompletedAt,
	}
}

// Execute is a default implementation that should be overridden
func (f *BaseFinalizer) Execute(ctx context.Context, service *types.Service) error {
	// Default implementation does nothing
	return nil
}

// Validate is a default implementation that should be overridden
func (f *BaseFinalizer) Validate(service *types.Service) error {
	// Default implementation always validates
	return nil
}
