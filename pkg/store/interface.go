// Package store provides a state storage interface and implementations for the Rune platform.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// ErrSkipUpdate, when returned by an UpdateFunc mutate callback, aborts the
// write and makes UpdateFunc return nil. It lets a conditional update decide —
// after seeing the freshly-read value — that no change is needed (e.g. the
// field is already at the desired value, or the resource is being deleted).
var ErrSkipUpdate = errors.New("store: skip update")

// Store defines the interface for state storage operations.
type Store interface {
	// Open initializes and opens the store.
	Open(path string) error

	// Close closes the store and releases resources.
	Close() error

	// Create creates a new resource.
	Create(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}) error

	// CreateResource creates a new resource.
	CreateResource(ctx context.Context, resourceType types.ResourceType, resource interface{}) error

	// Get retrieves a resource by type, namespace, and name.
	//
	// resource is DECODED INTO, not replaced: json.Unmarshal never zeroes
	// its destination, so anything the caller left on it that the stored
	// row omits survives — an absent `omitempty` scalar keeps its old
	// value, a non-nil map merges, a non-nil pointer is written through,
	// and a longer slice keeps its tail. Pass a fresh value. This is the
	// opposite of UpdateFunc below, which zeroes its target; the two look
	// alike at the call site and do not behave alike.
	Get(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}) error

	// GetInstanceByID retrieves an instance by namespace and instanceID.
	GetInstanceByID(ctx context.Context, namespace string, instanceID string) (*types.Instance, error)

	// List retrieves all resources of a given type in a namespace.
	// Decoded into rather than replaced — same caveat as Get.
	List(ctx context.Context, resourceType types.ResourceType, namespace string, resource interface{}) error

	// ListAll retrieves all resources of a given type in all namespaces.
	ListAll(ctx context.Context, resourceType types.ResourceType, resource interface{}) error

	// Update updates an existing resource.
	Update(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}, opts ...UpdateOption) error

	// UpdateFunc atomically reads the named resource into target, runs mutate
	// (which modifies target in place), and writes target back — all in a single
	// transaction, retrying on a write-write conflict so the mutation always
	// applies to the CURRENT stored state. This is the lost-update-safe
	// replacement for the Get → mutate-snapshot → Update pattern: two
	// controllers can each touch only their own fields without clobbering one
	// another. mutate MUST be a deterministic setter of target's fields — it may
	// be invoked more than once (once per retry) on a target that is zeroed and
	// re-read each time — so anything a rejected attempt wrote is gone, and any
	// state mutate needs across attempts must live outside target.
	// Returns a not-found error if the resource is absent.
	UpdateFunc(ctx context.Context, resourceType types.ResourceType, namespace string, name string, target interface{}, mutate func() error, opts ...UpdateOption) error

	// Delete deletes a resource.
	Delete(ctx context.Context, resourceType types.ResourceType, namespace string, name string) error

	// Watch sets up a watch for changes to resources of a given type.
	Watch(ctx context.Context, resourceType types.ResourceType, namespace string) (<-chan WatchEvent, error)

	// Transaction executes multiple operations in a single transaction.
	Transaction(ctx context.Context, fn func(tx Transaction) error) error

	// GetHistory retrieves historical versions of a resource.
	GetHistory(ctx context.Context, resourceType types.ResourceType, namespace string, name string) ([]HistoricalVersion, error)

	// GetVersion retrieves a specific version of a resource.
	GetVersion(ctx context.Context, resourceType types.ResourceType, namespace string, name string, version string) (interface{}, error)

	// GetOpts returns the store options/config in use
	GetOpts() StoreOptions
}

// Transaction represents a store transaction.
type Transaction interface {
	// Create creates a new resource within the transaction.
	Create(resourceType types.ResourceType, namespace string, name string, resource interface{}) error

	// Get retrieves a resource within the transaction.
	Get(resourceType types.ResourceType, namespace string, name string, resource interface{}) error

	// Update updates a resource within the transaction.
	Update(resourceType types.ResourceType, namespace string, name string, resource interface{}) error

	// Delete deletes a resource within the transaction.
	Delete(resourceType types.ResourceType, namespace string, name string) error
}

// WatchEventType defines the type of watch event.
type WatchEventType string

const (
	// WatchEventCreated indicates a resource was created.
	WatchEventCreated WatchEventType = "CREATED"

	// WatchEventUpdated indicates a resource was updated.
	WatchEventUpdated WatchEventType = "UPDATED"

	// WatchEventDeleted indicates a resource was deleted.
	WatchEventDeleted WatchEventType = "DELETED"
)

// WatchEvent represents a change to a resource.
type WatchEvent struct {
	// Type is the type of event (created, updated, deleted).
	Type WatchEventType

	// ResourceType is the type of resource affected.
	ResourceType types.ResourceType

	// Namespace is the namespace of the resource.
	Namespace string

	// Name is the name of the resource.
	Name string

	// Resource is the resource data.
	Resource interface{}

	// Source identifies who triggered this change (empty for external changes)
	Source EventSource
}

// HistoricalVersion represents a historical version of a resource.
type HistoricalVersion struct {
	// Version is the version identifier.
	Version string

	// Timestamp is when this version was created.
	Timestamp time.Time

	// Resource is the resource data for this version.
	Resource interface{}
}

type EventSource string

const (
	EventSourceOrchestrator     EventSource = "orchestrator"
	EventSourceAPI              EventSource = "api"
	EventSourceReconciler       EventSource = "reconciler"
	EventSourceHealthController EventSource = "health-controller"
)

// UpdateOption is a function that configures update options
type UpdateOption func(*UpdateOptions)

// UpdateOptions contains settings for update operations
type UpdateOptions struct {
	// Source identifies the origin of an update
	Source EventSource
}

// WithSource adds a source identifier to an update operation
func WithSource(source EventSource) UpdateOption {
	return func(o *UpdateOptions) {
		o.Source = source
	}
}

// WithOrchestrator marks an update as originating from the orchestrator
func WithOrchestrator() UpdateOption {
	return WithSource(EventSourceOrchestrator)
}

// WithReconciler marks an update as originating from the reconciler
func WithReconciler() UpdateOption {
	return WithSource(EventSourceReconciler)
}

// WithHealthController marks an update as originating from the health controller
func WithHealthController() UpdateOption {
	return WithSource(EventSourceHealthController)
}

// ParseUpdateOptions builds an UpdateOptions struct from a list of option functions
func ParseUpdateOptions(opts ...UpdateOption) UpdateOptions {
	options := UpdateOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	return options
}
