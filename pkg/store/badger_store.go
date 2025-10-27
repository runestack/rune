package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// Validate that BadgerStore implements the Store interface
var _ Store = &BadgerStore{}

// BadgerStore implements the Store interface using BadgerDB.
type BadgerStore struct {
	db         *badger.DB
	path       string
	logger     log.Logger
	watchChan  chan WatchEvent
	watchMu    sync.RWMutex
	watchConns map[string][]chan WatchEvent // key is resourceType:namespace

	// options
	opts StoreOptions
}

// NewBadgerStore creates a new BadgerDB-backed store.
func NewBadgerStore(logger log.Logger) *BadgerStore {
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("store")
	} else {
		logger = logger.WithComponent("store")
	}

	return &BadgerStore{
		logger:     logger,
		watchChan:  make(chan WatchEvent, 100), // Buffered channel for watch events
		watchConns: make(map[string][]chan WatchEvent),
	}
}

// NewBadgerStoreWithOptions creates a BadgerStore with options
func NewBadgerStoreWithOptions(logger log.Logger, opts StoreOptions) *BadgerStore {
	s := NewBadgerStore(logger)
	s.opts = opts
	return s
}

// Open opens the BadgerDB database.
func (s *BadgerStore) Open(path string) error {
	s.path = path

	// Configure BadgerDB options
	opts := badger.DefaultOptions(path)
	opts.Logger = &badgerLogAdapter{logger: s.logger}

	// Open the BadgerDB database
	db, err := badger.Open(opts)
	if err != nil {
		return fmt.Errorf("failed to open badger db: %w", err)
	}
	s.db = db

	// Start watch event handler
	go s.watchEventHandler()

	s.logger.Info("Rune store opened", log.Str("path", path))
	return nil
}

// GetOpts returns the configured store options
func (s *BadgerStore) GetOpts() StoreOptions { return s.opts }

// GetLimits returns configured limits for secrets and configs
func (s *BadgerStore) GetLimits() (Limits, Limits) { return s.opts.SecretLimits, s.opts.ConfigLimits }

// Close closes the BadgerDB database.
func (s *BadgerStore) Close() error {
	if s.db != nil {
		s.logger.Info("Closing Rune store", log.Str("path", s.path))

		// Clean up watch connections
		s.watchMu.Lock()
		close(s.watchChan)
		for _, conns := range s.watchConns {
			for _, ch := range conns {
				close(ch)
			}
		}
		s.watchConns = nil
		s.watchMu.Unlock()

		return s.db.Close()
	}
	return nil
}

func (s *BadgerStore) CreateResource(ctx context.Context, resourceType types.ResourceType, resource interface{}) error {
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

	s.logger.Debug("Creating resource",
		log.Any("resourceType", resourceType),
		log.Json("namespace", namespacedResource),
		log.Json("resource", resource))

	return s.Create(ctx, resourceType, namespacedResource.NamespacedName().Namespace, namespacedResource.NamespacedName().Name, resource)
}

// Create creates a new resource.
func (s *BadgerStore) Create(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	s.logger.Debug("Creating resource",
		log.Any("resourceType", resourceType),
		log.Str("namespace", namespace),
		log.Str("name", name))

	// Generate the key
	key := MakeKey(resourceType, namespace, name)

	// Serialize the resource
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to serialize resource: %w", err)
	}

	// Start a transaction
	txn := s.db.NewTransaction(true)
	defer txn.Discard()

	// Check if the resource already exists
	_, err = txn.Get(key)
	if err == nil {
		return fmt.Errorf("resource %s/%s/%s already exists", resourceType, namespace, name)
	} else if err != badger.ErrKeyNotFound {
		return fmt.Errorf("failed to check existing resource: %w", err)
	}

	// Store the resource
	err = txn.Set(key, data)
	if err != nil {
		return fmt.Errorf("failed to store resource: %w", err)
	}

	// Create initial version
	versionID := fmt.Sprintf("v%d", time.Now().UnixNano())
	versionKey := MakeVersionKey(resourceType, namespace, name, versionID)

	// Create version metadata
	version := struct {
		ID        string      `json:"id"`
		Timestamp time.Time   `json:"timestamp"`
		Resource  interface{} `json:"resource"`
	}{
		ID:        versionID,
		Timestamp: time.Now(),
		Resource:  resource,
	}

	// Serialize the version
	versionData, err := json.Marshal(version)
	if err != nil {
		return fmt.Errorf("failed to serialize version: %w", err)
	}

	// Store the version
	err = txn.Set(versionKey, versionData)
	if err != nil {
		return fmt.Errorf("failed to store version: %w", err)
	}

	// Commit the transaction
	if err := txn.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// Emit watch event
	s.emitWatchEvent(WatchEventCreated, resourceType, namespace, name, resource, "")

	return nil
}

// Get retrieves a resource.
func (s *BadgerStore) Get(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	// Generate the key
	key := MakeKey(resourceType, namespace, name)

	// Start a read-only transaction
	txn := s.db.NewTransaction(false)
	defer txn.Discard()

	// Get the item
	item, err := txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	} else if err != nil {
		return fmt.Errorf("failed to get resource: %w", err)
	}

	// Read the value
	return item.Value(func(val []byte) error {
		// Deserialize the resource
		return json.Unmarshal(val, resource)
	})
}

// GetInstance retrieves an instance by namespace and instanceID.
func (s *BadgerStore) GetInstanceByID(ctx context.Context, namespace string, instanceID string) (*types.Instance, error) {
	var instance types.Instance

	err := s.Get(ctx, types.ResourceTypeInstance, namespace, instanceID, &instance)
	if err != nil {
		return nil, err
	}

	return &instance, nil
}

// Update updates an existing resource.
func (s *BadgerStore) Update(ctx context.Context, resourceType types.ResourceType, namespace string, name string, resource interface{}, opts ...UpdateOption) error {
	s.logger.Debug("Updating resource",
		log.Any("resourceType", resourceType),
		log.Str("namespace", namespace),
		log.Str("name", name))

	// Parse options
	options := ParseUpdateOptions(opts...)

	// Generate the key
	key := MakeKey(resourceType, namespace, name)

	// Serialize the resource
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to serialize resource: %w", err)
	}

	// Start a transaction
	txn := s.db.NewTransaction(true)
	defer txn.Discard()

	// Check if the resource exists
	_, err = txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	} else if err != nil {
		return fmt.Errorf("failed to check existing resource: %w", err)
	}

	// Store the updated resource
	err = txn.Set(key, data)
	if err != nil {
		return fmt.Errorf("failed to store resource: %w", err)
	}

	// Create a new version
	versionID := fmt.Sprintf("v%d", time.Now().UnixNano())
	versionKey := MakeVersionKey(resourceType, namespace, name, versionID)

	// Create version metadata
	version := struct {
		ID        string      `json:"id"`
		Timestamp time.Time   `json:"timestamp"`
		Resource  interface{} `json:"resource"`
	}{
		ID:        versionID,
		Timestamp: time.Now(),
		Resource:  resource,
	}

	// Serialize the version
	versionData, err := json.Marshal(version)
	if err != nil {
		return fmt.Errorf("failed to serialize version: %w", err)
	}

	// Store the version
	err = txn.Set(versionKey, versionData)
	if err != nil {
		return fmt.Errorf("failed to store version: %w", err)
	}

	// Commit the transaction
	if err := txn.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// Emit watch event with source info
	s.emitWatchEvent(WatchEventUpdated, resourceType, namespace, name, resource, options.Source)

	return nil
}

// Delete deletes a resource.
func (s *BadgerStore) Delete(ctx context.Context, resourceType types.ResourceType, namespace string, name string) error {
	s.logger.Debug("Deleting resource",
		log.Any("resourceType", resourceType),
		log.Str("namespace", namespace),
		log.Str("name", name))

	// Generate the key
	key := MakeKey(resourceType, namespace, name)

	// Start a transaction
	txn := s.db.NewTransaction(true)
	defer txn.Discard()

	// Check if the resource exists and read it for the watch event
	item, err := txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	} else if err != nil {
		return fmt.Errorf("failed to check existing resource: %w", err)
	}

	// Read the value for watch event
	var resource interface{}
	err = item.Value(func(val []byte) error {
		return json.Unmarshal(val, &resource)
	})
	if err != nil {
		return fmt.Errorf("failed to deserialize resource: %w", err)
	}

	// Delete the resource
	err = txn.Delete(key)
	if err != nil {
		return fmt.Errorf("failed to delete resource: %w", err)
	}

	// Note: We don't delete versions to maintain history

	// Commit the transaction
	if err := txn.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// Emit watch event
	s.emitWatchEvent(WatchEventDeleted, resourceType, namespace, name, resource, "")

	return nil
}

// List retrieves all resources of a given type in a namespace.
func (s *BadgerStore) List(ctx context.Context, resourceType types.ResourceType, namespace string, resource interface{}) error {
	var resources []interface{}

	// Generate the prefix for scanning
	prefix := MakePrefix(resourceType, namespace)

	// Start a read-only transaction
	txn := s.db.NewTransaction(false)
	defer txn.Discard()

	// Create an iterator with the prefix
	opts := badger.DefaultIteratorOptions
	it := txn.NewIterator(opts)
	defer it.Close()

	// Iterate over items with the prefix
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		item := it.Item()

		// Read the value
		err := item.Value(func(val []byte) error {
			var resource interface{}
			if err := json.Unmarshal(val, &resource); err != nil {
				return fmt.Errorf("failed to deserialize resource: %w", err)
			}
			resources = append(resources, resource)
			return nil
		})
		if err != nil {
			return err
		}
	}

	s.logger.Debug("Found resources", log.Any("resourceType", resourceType), log.Int("count", len(resources)))
	return UnmarshalResource(resources, resource)
}

// ListAll retrieves all resources of a given type in all namespaces.
func (s *BadgerStore) ListAll(ctx context.Context, resourceType types.ResourceType, resource interface{}) error {
	return s.List(ctx, resourceType, "", resource)
}

// Transaction executes multiple operations in a single transaction.
func (s *BadgerStore) Transaction(ctx context.Context, fn func(tx Transaction) error) error {
	// Start a BadgerDB transaction
	txn := s.db.NewTransaction(true)
	defer txn.Discard()

	// Create our transaction wrapper
	storeTx := &BadgerTransaction{
		txn:   txn,
		store: s,
	}

	// Execute the function
	if err := fn(storeTx); err != nil {
		return err
	}

	// Commit the transaction
	return txn.Commit()
}

// GetHistory retrieves historical versions of a resource.
func (s *BadgerStore) GetHistory(ctx context.Context, resourceType types.ResourceType, namespace string, name string) ([]HistoricalVersion, error) {
	var versions []HistoricalVersion

	// Generate the prefix for scanning versions
	prefix := MakeVersionPrefix(resourceType, namespace, name)

	// Start a read-only transaction
	txn := s.db.NewTransaction(false)
	defer txn.Discard()

	// Create an iterator with the prefix
	opts := badger.DefaultIteratorOptions
	opts.Reverse = true // Get newest versions first
	it := txn.NewIterator(opts)
	defer it.Close()

	// Iterate over items with the prefix
	for it.Seek(append(prefix, 0xFF)); it.ValidForPrefix(prefix); it.Next() {
		item := it.Item()

		// Read the value
		err := item.Value(func(val []byte) error {
			var version struct {
				ID        string      `json:"id"`
				Timestamp time.Time   `json:"timestamp"`
				Resource  interface{} `json:"resource"`
			}

			if err := json.Unmarshal(val, &version); err != nil {
				return fmt.Errorf("failed to deserialize version: %w", err)
			}

			versions = append(versions, HistoricalVersion{
				Version:   version.ID,
				Timestamp: version.Timestamp,
				Resource:  version.Resource,
			})

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return versions, nil
}

// GetVersion retrieves a specific version of a resource.
func (s *BadgerStore) GetVersion(ctx context.Context, resourceType types.ResourceType, namespace string, name string, version string) (interface{}, error) {
	// Generate the version key
	versionKey := MakeVersionKey(resourceType, namespace, name, version)

	// Start a read-only transaction
	txn := s.db.NewTransaction(false)
	defer txn.Discard()

	// Get the item
	item, err := txn.Get(versionKey)
	if err == badger.ErrKeyNotFound {
		return nil, fmt.Errorf("version %s of resource %s/%s/%s not found", version, resourceType, namespace, name)
	} else if err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}

	// Read the value
	var result interface{}
	err = item.Value(func(val []byte) error {
		var version struct {
			ID        string      `json:"id"`
			Timestamp time.Time   `json:"timestamp"`
			Resource  interface{} `json:"resource"`
		}

		if err := json.Unmarshal(val, &version); err != nil {
			return fmt.Errorf("failed to deserialize version: %w", err)
		}

		result = version.Resource
		return nil
	})

	return result, err
}

// Watch sets up a watch for changes to resources of a given type.
func (s *BadgerStore) Watch(ctx context.Context, resourceType types.ResourceType, namespace string) (<-chan WatchEvent, error) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()

	// Create a filtered channel specific to this watch
	watchChan := make(chan WatchEvent, 10)

	// Add to active watches
	key := fmt.Sprintf("%s:%s", resourceType, namespace)

	// Check if watchConns is nil (store might be closed)
	if s.watchConns == nil {
		return nil, fmt.Errorf("store is closed, cannot create new watch")
	}

	s.watchConns[key] = append(s.watchConns[key], watchChan)

	// Remove from active watches when context is done
	go func() {
		<-ctx.Done()
		s.watchMu.Lock()
		defer s.watchMu.Unlock()

		// Find and remove this channel
		conns := s.watchConns[key]
		for i, ch := range conns {
			if ch == watchChan {
				// Remove this channel
				s.watchConns[key] = append(conns[:i], conns[i+1:]...)
				close(watchChan)
				break
			}
		}

		// Clean up the key if no more connections
		if len(s.watchConns[key]) == 0 {
			delete(s.watchConns, key)
		}
	}()

	return watchChan, nil
}

// emitWatchEvent sends a watch event to all watchers.
func (s *BadgerStore) emitWatchEvent(eventType WatchEventType, resourceType types.ResourceType, namespace string, name string, resource interface{}, source EventSource) {
	// Create the event
	event := WatchEvent{
		Type:         eventType,
		ResourceType: resourceType,
		Namespace:    namespace,
		Name:         name,
		Resource:     resource,
		Source:       source,
	}

	// Send the event to the channel (non-blocking)
	select {
	case s.watchChan <- event:
		// Event sent successfully
	default:
		// Channel is full, log a warning
		s.logger.Warn("Watch channel is full, dropping event",
			log.Any("type", eventType),
			log.Any("resourceType", resourceType),
			log.Str("namespace", namespace),
			log.Str("name", name))
	}
}

// watchEventHandler processes events from the main watch channel and distributes them to watchers.
func (s *BadgerStore) watchEventHandler() {
	for event := range s.watchChan {
		s.watchMu.RLock()

		// Dispatch to watchers for this exact resource type and namespace
		typeNsKey := fmt.Sprintf("%s:%s", event.ResourceType, event.Namespace)
		if conns, ok := s.watchConns[typeNsKey]; ok {
			for _, ch := range conns {
				select {
				case ch <- event:
					// Event sent
				default:
					// Channel is full, log and continue
					s.logger.Warn("Watch client channel is full, dropping event",
						log.Any("type", event.Type),
						log.Any("resourceType", event.ResourceType),
						log.Str("namespace", event.Namespace))
				}
			}
		}

		// Dispatch to watchers for all resource types in this namespace
		allTypesKey := fmt.Sprintf(":%s", event.Namespace)
		if conns, ok := s.watchConns[allTypesKey]; ok {
			for _, ch := range conns {
				select {
				case ch <- event:
					// Event sent
				default:
					// Channel is full, log and continue
					s.logger.Warn("Watch client channel is full, dropping event",
						log.Any("type", event.Type),
						log.Any("resourceType", event.ResourceType),
						log.Str("namespace", event.Namespace))
				}
			}
		}

		// Dispatch to watchers for this resource type in all namespaces
		typeAllNsKey := fmt.Sprintf("%s:", event.ResourceType)
		if conns, ok := s.watchConns[typeAllNsKey]; ok {
			for _, ch := range conns {
				select {
				case ch <- event:
					// Event sent
				default:
					// Channel is full, log and continue
					s.logger.Warn("Watch client channel is full, dropping event",
						log.Any("type", event.Type),
						log.Any("resourceType", event.ResourceType),
						log.Str("namespace", event.Namespace))
				}
			}
		}

		// Dispatch to watchers for all resource types and all namespaces
		allKey := ":"
		if conns, ok := s.watchConns[allKey]; ok {
			for _, ch := range conns {
				select {
				case ch <- event:
					// Event sent
				default:
					// Channel is full, log and continue
					s.logger.Warn("Watch client channel is full, dropping event",
						log.Any("type", event.Type),
						log.Any("resourceType", event.ResourceType),
						log.Str("namespace", event.Namespace))
				}
			}
		}

		s.watchMu.RUnlock()
	}
}

// BadgerTransaction implements the Transaction interface.
type BadgerTransaction struct {
	txn   *badger.Txn
	store *BadgerStore
}

// Create creates a resource within the transaction.
func (t *BadgerTransaction) Create(resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	// Generate the key
	key := MakeKey(resourceType, namespace, name)

	// Serialize the resource
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to serialize resource: %w", err)
	}

	// Check if the resource already exists
	_, err = t.txn.Get(key)
	if err == nil {
		return fmt.Errorf("resource %s/%s/%s already exists", resourceType, namespace, name)
	} else if err != badger.ErrKeyNotFound {
		return fmt.Errorf("failed to check existing resource: %w", err)
	}

	// Store the resource
	err = t.txn.Set(key, data)
	if err != nil {
		return fmt.Errorf("failed to store resource: %w", err)
	}

	// Create initial version
	versionID := fmt.Sprintf("v%d", time.Now().UnixNano())
	versionKey := MakeVersionKey(resourceType, namespace, name, versionID)

	// Create version metadata
	version := struct {
		ID        string      `json:"id"`
		Timestamp time.Time   `json:"timestamp"`
		Resource  interface{} `json:"resource"`
	}{
		ID:        versionID,
		Timestamp: time.Now(),
		Resource:  resource,
	}

	// Serialize the version
	versionData, err := json.Marshal(version)
	if err != nil {
		return fmt.Errorf("failed to serialize version: %w", err)
	}

	// Store the version
	err = t.txn.Set(versionKey, versionData)
	if err != nil {
		return fmt.Errorf("failed to store version: %w", err)
	}

	// Note: Watch events will be emitted after transaction commit

	return nil
}

// Get retrieves a resource within the transaction.
func (t *BadgerTransaction) Get(resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	// Generate the key
	key := MakeKey(resourceType, namespace, name)

	// Get the item
	item, err := t.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	} else if err != nil {
		return fmt.Errorf("failed to get resource: %w", err)
	}

	// Read the value
	return item.Value(func(val []byte) error {
		// Deserialize the resource
		return json.Unmarshal(val, resource)
	})
}

// Update updates a resource within the transaction.
func (t *BadgerTransaction) Update(resourceType types.ResourceType, namespace string, name string, resource interface{}) error {
	// Generate the key
	key := MakeKey(resourceType, namespace, name)

	// Serialize the resource
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to serialize resource: %w", err)
	}

	// Check if the resource exists
	_, err = t.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	} else if err != nil {
		return fmt.Errorf("failed to check existing resource: %w", err)
	}

	// Store the updated resource
	err = t.txn.Set(key, data)
	if err != nil {
		return fmt.Errorf("failed to store resource: %w", err)
	}

	// Create a new version
	versionID := fmt.Sprintf("v%d", time.Now().UnixNano())
	versionKey := MakeVersionKey(resourceType, namespace, name, versionID)

	// Create version metadata
	version := struct {
		ID        string      `json:"id"`
		Timestamp time.Time   `json:"timestamp"`
		Resource  interface{} `json:"resource"`
	}{
		ID:        versionID,
		Timestamp: time.Now(),
		Resource:  resource,
	}

	// Serialize the version
	versionData, err := json.Marshal(version)
	if err != nil {
		return fmt.Errorf("failed to serialize version: %w", err)
	}

	// Store the version
	err = t.txn.Set(versionKey, versionData)
	if err != nil {
		return fmt.Errorf("failed to store version: %w", err)
	}

	// Note: Watch events will be emitted after transaction commit

	return nil
}

// Delete deletes a resource within the transaction.
func (t *BadgerTransaction) Delete(resourceType types.ResourceType, namespace string, name string) error {
	// Generate the key
	key := MakeKey(resourceType, namespace, name)

	// Check if the resource exists
	_, err := t.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
	} else if err != nil {
		return fmt.Errorf("failed to check existing resource: %w", err)
	}

	// Delete the resource
	err = t.txn.Delete(key)
	if err != nil {
		return fmt.Errorf("failed to delete resource: %w", err)
	}

	// Note: We don't delete versions to maintain history
	// Note: Watch events will be emitted after transaction commit

	return nil
}

// badgerLogAdapter adapts our logger to BadgerDB's logger interface.
type badgerLogAdapter struct {
	logger log.Logger
}

// Errorf implements badger.Logger.
func (l *badgerLogAdapter) Errorf(format string, args ...interface{}) {
	l.logger.Errorf("BadgerDB: "+format, args...)
}

// Warningf implements badger.Logger.
func (l *badgerLogAdapter) Warningf(format string, args ...interface{}) {
	l.logger.Warnf("BadgerDB: "+format, args...)
}

// Infof implements badger.Logger.
func (l *badgerLogAdapter) Infof(format string, args ...interface{}) {
	l.logger.Debugf("BadgerDB: "+format, args...)
}

// Debugf implements badger.Logger.
func (l *badgerLogAdapter) Debugf(format string, args ...interface{}) {
	l.logger.Debugf("BadgerDB: "+format, args...)
}
