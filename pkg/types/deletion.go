package types

import (
	"time"

	"github.com/runestack/rune/pkg/worker"
)

// DeletionOperationStatus represents the status of a deletion operation
type DeletionOperationStatus string

const (
	// DeletionOperationStatusInitializing indicates the operation is being set up
	DeletionOperationStatusInitializing DeletionOperationStatus = "initializing"

	// DeletionOperationStatusDeletingInstances indicates instances are being deleted
	DeletionOperationStatusDeletingInstances DeletionOperationStatus = "deleting-instances"

	// DeletionOperationStatusRunningFinalizers indicates finalizers are executing
	DeletionOperationStatusRunningFinalizers DeletionOperationStatus = "running-finalizers"

	// DeletionOperationStatusCompleted indicates the operation completed successfully
	DeletionOperationStatusCompleted DeletionOperationStatus = "completed"

	// DeletionOperationStatusFailed indicates the operation failed
	DeletionOperationStatusFailed DeletionOperationStatus = "failed"
)

// FinalizerType represents the type of cleanup operation
type FinalizerType string

const (
	// Resource cleanup finalizers
	FinalizerTypeVolumeCleanup  FinalizerType = "volume-cleanup"
	FinalizerTypeNetworkCleanup FinalizerType = "network-cleanup"
	FinalizerTypeSecretCleanup  FinalizerType = "secret-cleanup"
	FinalizerTypeConfigCleanup  FinalizerType = "config-cleanup"

	// Service-related finalizers
	FinalizerTypeServiceDeregister   FinalizerType = "service-deregister"
	FinalizerTypeLoadBalancerCleanup FinalizerType = "load-balancer-cleanup"

	// Instance-related finalizers
	FinalizerTypeInstanceCleanup FinalizerType = "instance-cleanup"
	FinalizerTypeProcessCleanup  FinalizerType = "process-cleanup"
)

// FinalizerStatus represents the status of a finalizer
type FinalizerStatus string

const (
	FinalizerStatusPending   FinalizerStatus = "pending"
	FinalizerStatusRunning   FinalizerStatus = "running"
	FinalizerStatusCompleted FinalizerStatus = "completed"
	FinalizerStatusFailed    FinalizerStatus = "failed"
)

// FinalizerDependency represents a dependency between finalizers
type FinalizerDependency struct {
	// The type of finalizer that must complete before this one
	DependsOn FinalizerType `json:"depends_on"`

	// Whether the dependency is required (true) or optional (false)
	Required bool `json:"required"`
}

// Finalizer represents a cleanup operation that must complete before deletion
type Finalizer struct {
	// Unique identifier for the finalizer
	ID string `json:"id"`

	// Type of finalizer
	Type FinalizerType `json:"type"`

	// Status of the finalizer
	Status FinalizerStatus `json:"status"`

	// Error message if finalizer failed
	Error string `json:"error,omitempty"`

	// Dependencies this finalizer has on other finalizers
	Dependencies []FinalizerDependency `json:"dependencies,omitempty"`

	// Timestamps
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// DeletionOperation represents a deletion operation for a service
type DeletionOperation struct {
	// Unique identifier for the operation
	ID string `json:"id"`

	// Namespace the service belongs to
	Namespace string `json:"namespace"`

	// Name of the service being deleted
	ServiceName string `json:"service_name"`

	// Total number of instances to delete
	TotalInstances int `json:"total_instances"`

	// Number of instances deleted so far
	DeletedInstances int `json:"deleted_instances"`

	// Number of failed instance deletions
	FailedInstances int `json:"failed_instances"`

	// Time the operation started
	StartTime time.Time `json:"start_time"`

	// Time the operation ended (if completed/failed/cancelled)
	EndTime *time.Time `json:"end_time,omitempty"`

	// Current status of the operation (combines previous status and phase)
	Status DeletionOperationStatus `json:"status"`

	// Whether this is a dry run operation
	DryRun bool `json:"dry_run"`

	// Reason for failure/cancellation if applicable
	FailureReason string `json:"failure_reason,omitempty"`

	// List of pending cleanup operations
	PendingOperations []string `json:"pending_operations,omitempty"`

	// Estimated completion time
	EstimatedCompletion *time.Time `json:"estimated_completion,omitempty"`

	// Finalizers that need to complete before deletion (ordered by execution)
	Finalizers []Finalizer `json:"finalizers,omitempty"`
}

// FinalizerTimeoutConfig configures timeout behavior for finalizers
type FinalizerTimeoutConfig struct {
	// Default timeout for all finalizers
	DefaultTimeout time.Duration `json:"default_timeout"`

	// Timeout for specific finalizer types
	TypeTimeouts map[FinalizerType]time.Duration `json:"type_timeouts,omitempty"`

	// Whether to fail the entire operation if a finalizer times out
	FailOnTimeout bool `json:"fail_on_timeout"`
}

// DeletionRequest represents a request to delete a service
type DeletionRequest struct {
	// Namespace of the service
	Namespace string `json:"namespace"`

	// Name of the service
	Name string `json:"name"`

	// Force deletion without confirmation
	Force bool `json:"force"`

	// Timeout for graceful shutdown
	TimeoutSeconds int32 `json:"timeout_seconds"`

	// Detach and return immediately
	Detach bool `json:"detach"`

	// Dry run mode
	DryRun bool `json:"dry_run"`

	// Grace period for graceful shutdown
	GracePeriod int32 `json:"grace_period"`

	// Immediate deletion without graceful shutdown
	Now bool `json:"now"`

	// Don't error if service doesn't exist
	IgnoreNotFound bool `json:"ignore_not_found"`

	// Optional finalizers to run
	Finalizers []string `json:"finalizers,omitempty"`
}

// DeletionResponse represents the response from a deletion request
type DeletionResponse struct {
	// ID of the deletion operation
	DeletionID string `json:"deletion_id"`

	// Status of the deletion
	Status string `json:"status"`

	// Warning messages
	Warnings []string `json:"warnings,omitempty"`

	// Error messages
	Errors []string `json:"errors,omitempty"`

	// Whether cleanup was partial
	PartialCleanup bool `json:"partial_cleanup"`

	// Finalizers that will be executed
	Finalizers []Finalizer `json:"finalizers,omitempty"`
}

// DeletionTask represents a deletion task for the worker pool
type DeletionTask struct {
	*worker.BaseTask

	// Service to delete
	Service *Service `json:"service"`

	// Deletion request parameters
	Request *DeletionRequest `json:"request"`

	// Finalizers to execute
	FinalizerTypes []FinalizerType `json:"finalizer_types"`

	// Timeout configuration
	TimeoutConfig FinalizerTimeoutConfig `json:"timeout_config"`
}
