package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// Validate that TestStore implements the Store interface
var _ Store = &TestStore{}

// TestStore provides a simple in-memory implementation for testing purposes.
// Unlike MockStore, it doesn't require setting up expectations and is more convenient
// for basic tests that need a functional store.
type TestStore struct {
	data       map[types.ResourceType]map[string]map[string]interface{}
	history    map[types.ResourceType]map[string]map[string][]HistoricalVersion
	mutex      sync.RWMutex
	watchChans map[string][]chan WatchEvent
	watchMutex sync.RWMutex
	opened     bool

	// KEK handling for tests
	opts StoreOptions
}

// NewTestStore creates a new TestStore instance.
func NewTestStore() *TestStore {
	return &TestStore{
		data:       make(map[types.ResourceType]map[string]map[string]interface{}),
		history:    make(map[types.ResourceType]map[string]map[string][]HistoricalVersion),
		watchChans: make(map[string][]chan WatchEvent),
		opened:     true, // Consider it already opened for simplicity
	}
}

// NewTestStoreWithOptions creates a new TestStore instance with options
func NewTestStoreWithOptions(opts StoreOptions) *TestStore {
	return &TestStore{
		data:       make(map[types.ResourceType]map[string]map[string]interface{}),
		history:    make(map[types.ResourceType]map[string]map[string][]HistoricalVersion),
		watchChans: make(map[string][]chan WatchEvent),
		opened:     true, // Consider it already opened for simplicity
		opts:       opts,
	}
}

// Open implements the Store interface.
func (s *TestStore) Open(path string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.opened {
		return nil
	}

	s.opened = true
	return nil
}

// Close implements the Store interface.
func (s *TestStore) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.opened {
		return nil
	}

	// Close all watch channels
	s.watchMutex.Lock()
	for key, chans := range s.watchChans {
		for _, ch := range chans {
			close(ch)
		}
		delete(s.watchChans, key)
	}
	s.watchMutex.Unlock()

	s.opened = false
	return nil
}

// GetOpts returns test store options
func (s *TestStore) GetOpts() StoreOptions { return s.opts }

func (s *TestStore) CreateResource(ctx context.Context, resourceType types.ResourceType, resource interface{}) error {
	// Special case for Namespace resources
	if resourceType == types.ResourceTypeNamespace {
		namespace, ok := resource.(*types.Namespace)
		if !ok {
			return fmt.Errorf("expected Namespace type for namespace resource")
		}

		// Use the namespace name as both namespace and name
		// This effectively stores namespaces in a pseudo-namespace called "system"
		return s.Create(ctx, resourceType, "system", namespace.Name, namespace)
	}

	// Normal handling for other resource types
	namespacedResource, ok := resource.(types.NamespacedResource)
	if !ok {
		return fmt.Errorf("resource must implement NamespacedResource interface")
	}

	nn := namespacedResource.NamespacedName()
	return s.Create(ctx, resourceType, nn.Namespace, nn.Name, resource)
}

// Helper method for deep copying resources to avoid reference issues
func (s *TestStore) deepCopy(resource interface{}) interface{} {
	var copy interface{}

	switch v := resource.(type) {
	case *types.Service:
		// Create a copy to store
		copied := *v
		copy = &copied
	case *types.Instance:
		// Create a copy to store
		copied := *v
		copy = &copied
	case *types.ScalingOperation:
		// Create a copy to store
		copied := *v
		copy = &copied
	case *types.Namespace:
		// Create a copy to store
		copied := *v
		copy = &copied
	case *types.Policy:
		copied := *v
		copy = &copied
	default:
		// For other types, just use as-is (not ideal but works for basic types)
		copy = resource
	}

	return copy
}

// Create implements the Store interface.
func (s *TestStore) Create(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	fmt.Println("Create", resourceType, namespace, name, resource)

	if !s.opened {
		return errors.New("store is not opened")
	}

	fmt.Println("Before Create", resourceType, s.data[resourceType])
	// Initialize maps if they don't exist
	if _, exists := s.data[resourceType]; !exists {
		s.data[resourceType] = make(map[string]map[string]interface{})
	}
	if _, exists := s.history[resourceType]; !exists {
		s.history[resourceType] = make(map[string]map[string][]HistoricalVersion)
	}

	if _, exists := s.data[resourceType][namespace]; !exists {
		s.data[resourceType][namespace] = make(map[string]interface{})
	}
	if _, exists := s.history[resourceType][namespace]; !exists {
		s.history[resourceType][namespace] = make(map[string][]HistoricalVersion)
	}

	// Check if resource already exists
	if _, exists := s.data[resourceType][namespace][name]; exists {
		return fmt.Errorf("resource %s/%s/%s already exists", resourceType, namespace, name)
	}

	// Create a deep copy of the resource
	storeCopy := s.deepCopy(resource)

	// Store the copy
	s.data[resourceType][namespace][name] = storeCopy

	// Record history
	version := fmt.Sprintf("%d", time.Now().UnixNano())
	s.history[resourceType][namespace][name] = []HistoricalVersion{
		{
			Version:   version,
			Timestamp: time.Now(),
			Resource:  storeCopy,
		},
	}

	fmt.Println("After Create", s.data[resourceType])

	// Send watch event with a copy
	s.sendWatchEvent(resourceType, namespace, WatchEventCreated, name, storeCopy)

	return nil
}

// Get implements the Store interface.
func (s *TestStore) Get(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if !s.opened {
		return errors.New("store is not opened")
	}

	// Initialize the resource type map if it doesn't exist
	if _, exists := s.data[resourceType]; !exists {
		s.data[resourceType] = make(map[string]map[string]interface{})
	}

	// Initialize the namespace map if it doesn't exist
	if _, exists := s.data[resourceType][namespace]; !exists {
		s.data[resourceType][namespace] = make(map[string]interface{})
	}

	if data, exists := s.data[resourceType][namespace][name]; exists {
		// For testing purposes, we're implementing a simplified struct copying approach
		// This works with common test patterns where you pass a pointer to a struct

		// If it implements our Set interface, use that
		if setter, ok := resource.(interface{ Set(interface{}) }); ok {
			setter.Set(data)
			return nil
		}

		// For test convenience, let's try to handle common test types directly
		switch storedData := data.(type) {
		case map[string]interface{}:
			// If the target is a map
			if targetMap, ok := resource.(*map[string]interface{}); ok {
				*targetMap = storedData
				return nil
			}

		case *types.Service:
			// If the target is a Service pointer
			if targetSvc, ok := resource.(*types.Service); ok && storedData != nil {
				*targetSvc = *storedData
				return nil
			}

		case *types.Instance:
			// If the target is an Instance pointer
			if targetInst, ok := resource.(*types.Instance); ok && storedData != nil {
				*targetInst = *storedData
				return nil
			}

		case *types.ScalingOperation:
			// If the target is a ScalingOperation pointer
			if targetOp, ok := resource.(*types.ScalingOperation); ok && storedData != nil {
				*targetOp = *storedData
				return nil
			}

		case types.Service:
			// If stored as value but target is pointer
			if targetSvc, ok := resource.(*types.Service); ok {
				*targetSvc = storedData
				return nil
			}

		case *types.Configmap:
			if targetCfg, ok := resource.(*types.Configmap); ok && storedData != nil {
				*targetCfg = *storedData
				return nil
			}

		case types.Configmap:
			if targetCfg, ok := resource.(*types.Configmap); ok {
				*targetCfg = storedData
				return nil
			}

		case *types.Policy:
			if targetPol, ok := resource.(*types.Policy); ok && storedData != nil {
				*targetPol = *storedData
				return nil
			}

		case types.Policy:
			if targetPol, ok := resource.(*types.Policy); ok {
				*targetPol = storedData
				return nil
			}

		case *types.StoredSecret:
			// If the target is a Secret pointer
			if targetSec, ok := resource.(*types.StoredSecret); ok && storedData != nil {
				*targetSec = *storedData
				return nil
			}

		case types.StoredSecret:
			// If stored as value but target is pointer
			if targetSec, ok := resource.(*types.StoredSecret); ok {
				*targetSec = storedData
				return nil
			}

		case *types.Secret:
			// If the target is a Secret pointer
			if targetSec, ok := resource.(*types.Secret); ok && storedData != nil {
				*targetSec = *storedData
				return nil
			}

		case types.Secret:
			// If stored as value but target is pointer
			if targetSec, ok := resource.(*types.Secret); ok {
				*targetSec = storedData
				return nil
			}

		case types.Instance:
			// If stored as value but target is pointer
			if targetInst, ok := resource.(*types.Instance); ok {
				*targetInst = storedData
				return nil
			}

		case types.ScalingOperation:
			// If stored as value but target is pointer
			if targetOp, ok := resource.(*types.ScalingOperation); ok {
				*targetOp = storedData
				return nil
			}

		case *types.DeletionOperation:
			// If the target is a DeletionOperation pointer
			if targetOp, ok := resource.(*types.DeletionOperation); ok && storedData != nil {
				*targetOp = *storedData
				return nil
			}

		case types.DeletionOperation:
			// If stored as value but target is pointer
			if targetOp, ok := resource.(*types.DeletionOperation); ok {
				*targetOp = storedData
				return nil
			}

		case *types.User:
			if targetUser, ok := resource.(*types.User); ok && storedData != nil {
				*targetUser = *storedData
				return nil
			}

		case types.User:
			if targetUser, ok := resource.(*types.User); ok {
				*targetUser = storedData
				return nil
			}

		}

		// Store the data - for testing we assume this will work
		// This is intentionally simplified for testing purposes
		return fmt.Errorf("cannot set resource data: incompatible types (try using a pointer to the correct type)")
	}

	return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
}

// List implements the Store interface.
func (s *TestStore) List(ctx context.Context, resourceType types.ResourceType, namespace string, resource interface{}) error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	fmt.Println("Before List", resourceType, s.data[resourceType])

	if !s.opened {
		return errors.New("store is not opened")
	}

	if _, exists := s.data[resourceType]; !exists {
		result := make([]interface{}, 0)
		return UnmarshalResource(result, resource)
	}

	if _, exists := s.data[resourceType][namespace]; !exists {
		return fmt.Errorf("namespace %s not found in resource type %s", namespace, resourceType)
	}

	result := make([]interface{}, 0, len(s.data[resourceType][namespace]))
	for _, resource := range s.data[resourceType][namespace] {
		result = append(result, resource)
	}

	return UnmarshalResource(result, resource)
}

// ListAll retrieves all resources of a given type in all namespaces.
func (s *TestStore) ListAll(ctx context.Context, resourceType types.ResourceType, resource interface{}) error {
	return s.List(ctx, resourceType, "", resource)
}

// Update implements the Store interface.
func (s *TestStore) Update(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}, opts ...UpdateOption) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Parse update options
	options := ParseUpdateOptions(opts...)

	if !s.opened {
		return errors.New("store is not opened")
	}

	if _, exists := s.data[resourceType]; !exists {
		return fmt.Errorf("resource type %s not found", resourceType)
	}

	if _, exists := s.data[resourceType][namespace]; !exists {
		return fmt.Errorf("namespace %s not found in resource type %s", namespace, resourceType)
	}

	if _, exists := s.data[resourceType][namespace][name]; !exists {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	}

	// Create a deep copy of the resource
	storeCopy := s.deepCopy(resource)

	// Update resource
	s.data[resourceType][namespace][name] = storeCopy

	// Record history
	version := fmt.Sprintf("%d", time.Now().UnixNano())
	s.history[resourceType][namespace][name] = append(s.history[resourceType][namespace][name], HistoricalVersion{
		Version:   version,
		Timestamp: time.Now(),
		Resource:  storeCopy,
	})

	// Send watch event with source info and a copy
	s.sendWatchEventWithSource(resourceType, namespace, WatchEventUpdated, name, storeCopy, options.Source)

	return nil
}

// Delete implements the Store interface.
func (s *TestStore) Delete(ctx context.Context, resourceType types.ResourceType, namespace string, name string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.opened {
		return errors.New("store is not opened")
	}

	if _, exists := s.data[resourceType]; !exists {
		return fmt.Errorf("resource type %s not found", resourceType)
	}

	if _, exists := s.data[resourceType][namespace]; !exists {
		return fmt.Errorf("namespace %s not found in resource type %s", namespace, resourceType)
	}

	if _, exists := s.data[resourceType][namespace][name]; !exists {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	}

	// Get resource before deleting for watch event
	resource := s.data[resourceType][namespace][name]

	// Create a deep copy for the watch event
	eventCopy := s.deepCopy(resource)

	// Delete resource
	delete(s.data[resourceType][namespace], name)

	// Send watch event with a copy
	s.sendWatchEvent(resourceType, namespace, WatchEventDeleted, name, eventCopy)

	return nil
}

// Watch implements the Store interface.
func (s *TestStore) Watch(ctx context.Context, resourceType types.ResourceType, namespace string) (<-chan WatchEvent, error) {
	s.watchMutex.Lock()
	defer s.watchMutex.Unlock()

	fmt.Printf("Setting up watch for %s, namespace=%s\n", resourceType, namespace)

	if !s.opened {
		return nil, errors.New("store is not opened")
	}

	// Create a buffered channel to avoid blocking
	ch := make(chan WatchEvent, 100)

	// Generate a watch key
	watchKey := fmt.Sprintf("%s/%s", resourceType, namespace)

	// Add the channel to the watch channels
	if _, exists := s.watchChans[watchKey]; !exists {
		s.watchChans[watchKey] = make([]chan WatchEvent, 0)
		fmt.Printf("Created new watcher list for %s\n", watchKey)
	}
	s.watchChans[watchKey] = append(s.watchChans[watchKey], ch)
	fmt.Printf("Added watcher for %s, now have %d watchers\n", watchKey, len(s.watchChans[watchKey]))

	// Set up cancellation handling
	go func() {
		<-ctx.Done()
		s.watchMutex.Lock()
		defer s.watchMutex.Unlock()

		// Find and remove the channel
		for i, c := range s.watchChans[watchKey] {
			if c == ch {
				s.watchChans[watchKey] = append(s.watchChans[watchKey][:i], s.watchChans[watchKey][i+1:]...)
				close(ch)
				fmt.Printf("Watch context cancelled, removed watcher for %s\n", watchKey)
				break
			}
		}
	}()

	return ch, nil
}

// GetHistory implements the Store interface.
func (s *TestStore) GetHistory(ctx context.Context, resourceType types.ResourceType, namespace string, name string) ([]HistoricalVersion, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if !s.opened {
		return nil, errors.New("store is not opened")
	}

	if _, exists := s.history[resourceType]; !exists {
		return []HistoricalVersion{}, nil
	}

	if _, exists := s.history[resourceType][namespace]; !exists {
		return []HistoricalVersion{}, nil
	}

	if versions, exists := s.history[resourceType][namespace][name]; exists {
		// Return a copy to avoid mutation
		result := make([]HistoricalVersion, len(versions))
		copy(result, versions)
		return result, nil
	}

	return []HistoricalVersion{}, nil
}

// GetVersion implements the Store interface.
func (s *TestStore) GetVersion(ctx context.Context, resourceType types.ResourceType, namespace string, name string, version string) (interface{}, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if !s.opened {
		return nil, errors.New("store is not opened")
	}

	if _, exists := s.history[resourceType]; !exists {
		return nil, fmt.Errorf("resource type %s not found", resourceType)
	}

	if _, exists := s.history[resourceType][namespace]; !exists {
		return nil, fmt.Errorf("namespace %s not found in resource type %s", namespace, resourceType)
	}

	if versions, exists := s.history[resourceType][namespace][name]; exists {
		for _, v := range versions {
			if v.Version == version {
				return v.Resource, nil
			}
		}
		return nil, fmt.Errorf("version %s not found for resource %s/%s/%s", version, resourceType, namespace, name)
	}

	return nil, fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
}

// Transaction implements the Store interface.
func (s *TestStore) Transaction(ctx context.Context, fn func(tx Transaction) error) error {
	if !s.opened {
		return errors.New("store is not opened")
	}

	// Create a transaction
	tx := &testTransaction{
		store: s,
		ctx:   ctx,
	}

	// Execute the transaction function
	return fn(tx)
}

// Helper method to send watch events with source info
func (s *TestStore) sendWatchEventWithSource(resourceType types.ResourceType, namespace string, eventType WatchEventType, name string, resource interface{}, source EventSource) {
	s.watchMutex.RLock()
	defer s.watchMutex.RUnlock()

	// Create the event
	event := WatchEvent{
		Type:         eventType,
		ResourceType: resourceType,
		Namespace:    namespace,
		Name:         name,
		Resource:     resource,
		Source:       source,
	}

	// First try exact namespace match
	exactWatchKey := fmt.Sprintf("%s/%s", resourceType, namespace)
	fmt.Printf("Sending watch event: %s, %s, %s, key=%s\n", eventType, resourceType, name, exactWatchKey)

	// Check for exact namespace match watchers
	if chans, exists := s.watchChans[exactWatchKey]; exists {
		fmt.Printf("Found %d watchers for exact match %s\n", len(chans), exactWatchKey)
		for _, ch := range chans {
			// Non-blocking send
			select {
			case ch <- event:
				fmt.Printf("Event sent successfully to exact watcher for %s\n", exactWatchKey)
			default:
				fmt.Printf("Channel full, skipping event for exact watcher %s\n", exactWatchKey)
			}
		}
	} else {
		fmt.Printf("No exact watchers found for %s\n", exactWatchKey)
	}

	// Also check for resource-wide watchers (with empty namespace)
	wildcardWatchKey := fmt.Sprintf("%s/", resourceType)
	if namespace != "" && wildcardWatchKey != exactWatchKey {
		fmt.Printf("Also checking for wildcard watchers %s\n", wildcardWatchKey)

		if chans, exists := s.watchChans[wildcardWatchKey]; exists {
			fmt.Printf("Found %d wildcard watchers for %s\n", len(chans), wildcardWatchKey)
			for _, ch := range chans {
				// Non-blocking send
				select {
				case ch <- event:
					fmt.Printf("Event sent successfully to wildcard watcher for %s\n", wildcardWatchKey)
				default:
					fmt.Printf("Channel full, skipping event for wildcard watcher %s\n", wildcardWatchKey)
				}
			}
		} else {
			fmt.Printf("No wildcard watchers found for %s\n", wildcardWatchKey)
		}
	}
}

// Helper method to send watch events
func (s *TestStore) sendWatchEvent(resourceType types.ResourceType, namespace string, eventType WatchEventType, name string, resource interface{}) {
	s.sendWatchEventWithSource(resourceType, namespace, eventType, name, resource, "")
}

// testTransaction implements the Transaction interface
type testTransaction struct {
	store *TestStore
	ctx   context.Context
}

// Create implements the Transaction interface
func (tx *testTransaction) Create(resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	return tx.store.Create(tx.ctx, resourceType, namespace, name, resource)
}

// Get implements the Transaction interface
func (tx *testTransaction) Get(resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	return tx.store.Get(tx.ctx, resourceType, namespace, name, resource)
}

// Update implements the Transaction interface
func (tx *testTransaction) Update(resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	return tx.store.Update(tx.ctx, resourceType, namespace, name, resource)
}

// Delete implements the Transaction interface
func (tx *testTransaction) Delete(resourceType types.ResourceType, namespace string, name string) error {
	return tx.store.Delete(tx.ctx, resourceType, namespace, name)
}

// Helper functions for testing

// SetupTestData adds predefined test data to the store
func (s *TestStore) SetupTestData(resources map[types.ResourceType]map[string]map[string]interface{}) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for resourceType, namespaces := range resources {
		if _, exists := s.data[resourceType]; !exists {
			s.data[resourceType] = make(map[string]map[string]interface{})
			s.history[resourceType] = make(map[string]map[string][]HistoricalVersion)
		}

		for namespace, items := range namespaces {
			if _, exists := s.data[resourceType][namespace]; !exists {
				s.data[resourceType][namespace] = make(map[string]interface{})
				s.history[resourceType][namespace] = make(map[string][]HistoricalVersion)
			}

			for name, resource := range items {
				s.data[resourceType][namespace][name] = resource
				s.history[resourceType][namespace][name] = []HistoricalVersion{
					{
						Version:   "1",
						Timestamp: time.Now(),
						Resource:  resource,
					},
				}
			}
		}
	}

	return nil
}

// Reset clears all data in the store
func (s *TestStore) Reset() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.data = make(map[types.ResourceType]map[string]map[string]interface{})
	s.history = make(map[types.ResourceType]map[string]map[string][]HistoricalVersion)

	// Close all watch channels
	s.watchMutex.Lock()
	for key, chans := range s.watchChans {
		for _, ch := range chans {
			close(ch)
		}
		delete(s.watchChans, key)
	}
	s.watchMutex.Unlock()
}

// Additional helper methods for more convenient testing

// CreateService adds a service to the test store
func (s *TestStore) CreateService(ctx context.Context, service *types.Service) error {
	if service.Namespace == "" {
		service.Namespace = "default"
	}
	return s.Create(ctx, types.ResourceTypeService, service.Namespace, service.Name, service)
}

// GetService retrieves a service from the test store
func (s *TestStore) GetService(ctx context.Context, namespace, name string) (*types.Service, error) {
	if namespace == "" {
		namespace = "default"
	}

	service := &types.Service{}
	err := s.Get(ctx, types.ResourceTypeService, namespace, name, service)
	if err != nil {
		return nil, err
	}
	return service, nil
}

// CreateInstance adds an instance to the test store
func (s *TestStore) CreateInstance(ctx context.Context, instance *types.Instance) error {
	if instance.Namespace == "" {
		instance.Namespace = "default"
	}
	return s.Create(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance)
}

// GetInstance retrieves an instance from the test store
func (s *TestStore) GetInstanceByID(ctx context.Context, namespace, id string) (*types.Instance, error) {
	if namespace == "" {
		namespace = "default"
	}

	instance := &types.Instance{}
	err := s.Get(ctx, types.ResourceTypeInstance, namespace, id, instance)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

// ListServices returns all services in a namespace
func (s *TestStore) ListServices(ctx context.Context, namespace string) ([]*types.Service, error) {
	if namespace == "" {
		namespace = "default"
	}

	var services []*types.Service
	err := s.List(ctx, types.ResourceTypeService, namespace, &services)
	if err != nil {
		return nil, err
	}

	return services, nil
}

// ListInstances returns all instances in a namespace
func (s *TestStore) ListInstances(ctx context.Context, namespace string) ([]types.Instance, error) {
	if namespace == "" {
		namespace = "default"
	}

	var instances []types.Instance
	err := s.List(ctx, types.ResourceTypeInstance, namespace, &instances)
	if err != nil {
		return nil, err
	}

	return instances, nil
}
