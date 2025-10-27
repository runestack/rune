package finalizers

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/types"
)

// FinalizerFactory is a function that creates a new finalizer instance
type FinalizerFactory func() FinalizerInterface

// FinalizerRegistry manages finalizer types and creates instances
type FinalizerRegistry struct {
	mu        sync.RWMutex
	factories map[types.FinalizerType]FinalizerFactory
}

// NewFinalizerRegistry creates a new finalizer registry
func NewFinalizerRegistry() *FinalizerRegistry {
	return &FinalizerRegistry{
		factories: make(map[types.FinalizerType]FinalizerFactory),
	}
}

// Register registers a finalizer factory for a given type
func (r *FinalizerRegistry) Register(finalizerType types.FinalizerType, factory FinalizerFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[finalizerType] = factory
}

// Create creates a new finalizer instance of the given type
func (r *FinalizerRegistry) Create(finalizerType types.FinalizerType) (FinalizerInterface, error) {
	r.mu.RLock()
	factory, exists := r.factories[finalizerType]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("finalizer type %s not registered", finalizerType)
	}

	return factory(), nil
}

// Get creates a finalizer instance (alias for Create for backward compatibility)
func (r *FinalizerRegistry) Get(finalizerType types.FinalizerType) (FinalizerInterface, bool) {
	finalizer, err := r.Create(finalizerType)
	return finalizer, err == nil
}

// ListTypes returns all registered finalizer types
func (r *FinalizerRegistry) ListTypes() []types.FinalizerType {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]types.FinalizerType, 0, len(r.factories))
	for finalizerType := range r.factories {
		types = append(types, finalizerType)
	}
	return types
}

// IsRegistered checks if a finalizer type is registered
func (r *FinalizerRegistry) IsRegistered(finalizerType types.FinalizerType) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.factories[finalizerType]
	return exists
}

// NewBaseFinalizer creates a new base finalizer with the given type
func NewBaseFinalizer(finalizerType types.FinalizerType, dependencies []types.FinalizerDependency) *BaseFinalizer {
	now := time.Now()
	return &BaseFinalizer{
		ID:           uuid.New().String(),
		Type:         finalizerType,
		Status:       types.FinalizerStatusPending,
		Dependencies: dependencies,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}
