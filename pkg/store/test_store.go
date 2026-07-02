package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

// deepCopy returns a copy of the resource so stored values don't alias caller
// memory. For any pointer-to-struct it performs the same shallow value copy
// the old per-type switch did (`copied := *v`), but generically — every
// resource type, present and future, gets identical treatment without
// registration. (Deliberately NOT a JSON round-trip: that would drop
// unexported and json:"-" fields such as the embedded NamespacedResource.)
func (s *TestStore) deepCopy(resource interface{}) interface{} {
	rv := reflect.ValueOf(resource)
	if rv.Kind() == reflect.Ptr && !rv.IsNil() {
		out := reflect.New(rv.Elem().Type())
		out.Elem().Set(rv.Elem())
		return out.Interface()
	}
	// Non-pointer values (maps, plain structs) are stored as-is, matching the
	// old default branch.
	return resource
}

// Create implements the Store interface.
func (s *TestStore) Create(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.opened {
		return errors.New("store is not opened")
	}

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

	// Send watch event with an independent deep-copy so subscribers that mutate
	// the received resource in-place (e.g. controllers) do not race with later
	// readers of the stored value via Get.
	s.sendWatchEvent(resourceType, namespace, WatchEventCreated, name, s.deepCopy(storeCopy))

	return nil
}

// Get implements the Store interface.
func (s *TestStore) Get(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if !s.opened {
		return errors.New("store is not opened")
	}

	// Read-only path: do not mutate s.data here. Mutating maps under RLock
	// races with other Get callers and with Update/Create writers when the
	// race detector is enabled.
	typeBucket, ok := s.data[resourceType]
	if !ok {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	}
	nsBucket, ok := typeBucket[namespace]
	if !ok {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	}

	if data, exists := nsBucket[name]; exists {
		// If the target implements the Set interface, use that.
		if setter, ok := resource.(interface{ Set(interface{}) }); ok {
			setter.Set(data)
			return nil
		}

		// Generic pointer assignment — replaces the old hand-maintained
		// per-type switch (which silently returned "not found" for any
		// resource type nobody had registered). Stored values are either
		// *T (the deepCopy path) or plain values; the caller passes *T.
		tv := reflect.ValueOf(resource)
		if tv.Kind() == reflect.Ptr && !tv.IsNil() {
			te := tv.Elem()
			dv := reflect.ValueOf(data)
			// stored *T -> target *T  (the `*target = *stored` cases)
			if dv.Kind() == reflect.Ptr && !dv.IsNil() && dv.Elem().Type() == te.Type() {
				te.Set(dv.Elem())
				return nil
			}
			// stored T -> target *T  (the value-stored cases, incl. maps)
			if dv.IsValid() && dv.Type() == te.Type() {
				te.Set(dv)
				return nil
			}
		}

		// Shape mismatch (e.g. stored as map[string]interface{}, typed
		// target): JSON round-trip recovers the typed form.
		b, err := json.Marshal(data)
		if err == nil && json.Unmarshal(b, resource) == nil {
			return nil
		}
		return fmt.Errorf("cannot set resource data: incompatible types (try using a pointer to the correct type)")
	}

	return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
}

// List implements the Store interface.
func (s *TestStore) List(ctx context.Context, resourceType types.ResourceType, namespace string, resource interface{}) error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

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

// ListAll retrieves all resources of a given type in all namespaces. Unlike
// List, a missing namespace map is not an error — it iterates every namespace
// under the resource type, matching BadgerStore's prefix-scan semantics.
// (Previously this delegated to List(ctx, type, "", ...), which errored unless
// resources had been stored under the literal "" namespace — so ListAll never
// worked on TestStore; callers only survived because they logged and ignored
// the error.)
func (s *TestStore) ListAll(ctx context.Context, resourceType types.ResourceType, resource interface{}) error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if !s.opened {
		return errors.New("store is not opened")
	}

	result := make([]interface{}, 0)
	for _, nsResources := range s.data[resourceType] {
		for _, r := range nsResources {
			result = append(result, r)
		}
	}
	return UnmarshalResource(result, resource)
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

	// Send watch event with source info and an independent deep-copy so
	// subscribers that mutate the received resource in-place (e.g.
	// controllers) do not race with later readers of the stored value via
	// Get.
	s.sendWatchEventWithSource(resourceType, namespace, WatchEventUpdated, name, s.deepCopy(storeCopy), options.Source)

	return nil
}

// UpdateFunc implements the Store interface. The write lock makes the
// read-modify-write atomic, so no retry is needed. Mirrors BadgerStore's
// contract: read current into target, run mutate, write target back.
func (s *TestStore) UpdateFunc(ctx context.Context, resourceType types.ResourceType, namespace string, name string, target interface{}, mutate func() error, opts ...UpdateOption) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
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
	stored, exists := s.data[resourceType][namespace][name]
	if !exists {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	}

	// Load the current value into target (JSON round-trip, same as Get).
	b, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("failed to serialize stored resource: %w", err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		return fmt.Errorf("failed to deserialize resource: %w", err)
	}

	if err := mutate(); err != nil {
		if errors.Is(err, ErrSkipUpdate) {
			return nil
		}
		return err
	}

	storeCopy := s.deepCopy(target)
	s.data[resourceType][namespace][name] = storeCopy
	version := fmt.Sprintf("%d", time.Now().UnixNano())
	s.history[resourceType][namespace][name] = append(s.history[resourceType][namespace][name], HistoricalVersion{
		Version:   version,
		Timestamp: time.Now(),
		Resource:  storeCopy,
	})
	s.sendWatchEventWithSource(resourceType, namespace, WatchEventUpdated, name, s.deepCopy(storeCopy), options.Source)
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
	}
	s.watchChans[watchKey] = append(s.watchChans[watchKey], ch)

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

	// Check for exact namespace match watchers
	if chans, exists := s.watchChans[exactWatchKey]; exists {
		for _, ch := range chans {
			// Non-blocking send
			select {
			case ch <- event:
			default:
			}
		}
	}

	// Also check for resource-wide watchers (with empty namespace)
	wildcardWatchKey := fmt.Sprintf("%s/", resourceType)
	if namespace != "" && wildcardWatchKey != exactWatchKey {

		if chans, exists := s.watchChans[wildcardWatchKey]; exists {
			for _, ch := range chans {
				// Non-blocking send
				select {
				case ch <- event:
				default:
				}
			}
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
