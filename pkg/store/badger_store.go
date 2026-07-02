package store

import (
	"context"
	"encoding/json"
	"errors"
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
	watchMu    sync.RWMutex
	watchConns map[string][]*watchSub // key is resourceType:namespace

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
		watchConns: make(map[string][]*watchSub),
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

	s.logger.Info("Rune store opened", log.Str("path", path))
	return nil
}

// GetOpts returns the configured store options
func (s *BadgerStore) GetOpts() StoreOptions { return s.opts }

// DB returns the underlying *badger.DB. Intended for in-process
// subsystems (notably pkg/store/orderedlog) that need to share the
// same Badger instance for atomic writes against reserved key
// prefixes. Callers MUST NOT write to keys outside their reserved
// prefix; the orderedlog seam lint enforces this.
func (s *BadgerStore) DB() *badger.DB { return s.db }

// GetLimits returns configured limits for secrets and configs
func (s *BadgerStore) GetLimits() (Limits, Limits) { return s.opts.SecretLimits, s.opts.ConfigLimits }

// Close closes the BadgerDB database.
func (s *BadgerStore) Close() error {
	if s.db != nil {
		s.logger.Info("Closing Rune store", log.Str("path", s.path))

		// Clean up watch connections: stop every subscriber's pump, which
		// closes its out-channel so ranging consumers observe the shutdown.
		s.watchMu.Lock()
		for _, conns := range s.watchConns {
			for _, sub := range conns {
				sub.stop()
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

// updateFuncMaxRetries bounds the optimistic-concurrency retry loop. Conflicts
// are resolved in microseconds, so this is a runaway backstop, not a tuning
// knob — real contention on one key clears in 1-2 retries.
const updateFuncMaxRetries = 100

// UpdateFunc runs read → mutate → write inside one Badger transaction. Because
// the read (txn.Get) joins the transaction's read set, Badger's serializable
// snapshot isolation rejects the commit with ErrConflict if another writer
// changed the key in between — at which point we re-read and re-apply mutate.
// The plain Update() path can't do this: its read (the caller's store.Get) is a
// separate transaction, so concurrent writers silently clobber each other.
func (s *BadgerStore) UpdateFunc(ctx context.Context, resourceType types.ResourceType, namespace string, name string, target interface{}, mutate func() error, opts ...UpdateOption) error {
	options := ParseUpdateOptions(opts...)
	key := MakeKey(resourceType, namespace, name)

	for attempt := 0; ; attempt++ {
		err := func() error {
			txn := s.db.NewTransaction(true)
			defer txn.Discard()

			// Read current into target — this enrolls the key in the read set.
			item, err := txn.Get(key)
			if err == badger.ErrKeyNotFound {
				return fmt.Errorf("resource %s/%s/%s not found", resourceType, namespace, name)
			} else if err != nil {
				return fmt.Errorf("failed to get resource: %w", err)
			}
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, target) }); err != nil {
				return fmt.Errorf("failed to deserialize resource: %w", err)
			}

			// Apply the caller's mutation to the freshly-read target. A
			// mutate that returns ErrSkipUpdate aborts the write (handled below).
			if err := mutate(); err != nil {
				return err
			}

			data, err := json.Marshal(target)
			if err != nil {
				return fmt.Errorf("failed to serialize resource: %w", err)
			}
			if err := txn.Set(key, data); err != nil {
				return fmt.Errorf("failed to store resource: %w", err)
			}

			// Mirror Update's version-history write.
			versionID := fmt.Sprintf("v%d", time.Now().UnixNano())
			versionKey := MakeVersionKey(resourceType, namespace, name, versionID)
			version := struct {
				ID        string      `json:"id"`
				Timestamp time.Time   `json:"timestamp"`
				Resource  interface{} `json:"resource"`
			}{ID: versionID, Timestamp: time.Now(), Resource: target}
			versionData, err := json.Marshal(version)
			if err != nil {
				return fmt.Errorf("failed to serialize version: %w", err)
			}
			if err := txn.Set(versionKey, versionData); err != nil {
				return fmt.Errorf("failed to store version: %w", err)
			}

			return txn.Commit()
		}()

		if err == nil {
			s.emitWatchEvent(WatchEventUpdated, resourceType, namespace, name, target, options.Source)
			return nil
		}
		// The mutate aborted the write — success, no event.
		if errors.Is(err, ErrSkipUpdate) {
			return nil
		}
		// Retry only on a genuine write-write conflict.
		if errors.Is(err, badger.ErrConflict) && attempt < updateFuncMaxRetries {
			continue
		}
		return err
	}
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

// watchBacklogWarn is the per-subscriber backlog depth at which we log a
// warning. Crossing it means a watch consumer is draining slower than events
// are produced; the backlog is unbounded (we never drop), so a persistently
// slow/stuck consumer would grow memory — the warning surfaces that.
const watchBacklogWarn = 1024

// watchSub is a single watch subscription. A dedicated pump goroutine bridges
// an unbounded internal backlog (buf) to the consumer's out-channel. This makes
// delivery non-lossy: emitWatchEvent enqueues without ever blocking or dropping,
// a slow consumer grows only its OWN backlog (never stalling the emit path or
// other subscribers), and events are delivered in order per subscriber.
type watchSub struct {
	out    chan WatchEvent
	notify chan struct{} // wakes the pump when buf gains items
	done   chan struct{} // closed by stop() to terminate the pump

	mu  sync.Mutex
	buf []WatchEvent

	stopOnce sync.Once
	logger   log.Logger
	key      string
	warned   bool
}

func newWatchSub(logger log.Logger, key string) *watchSub {
	sub := &watchSub{
		out:    make(chan WatchEvent),
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
		logger: logger,
		key:    key,
	}
	go sub.pump()
	return sub
}

// enqueue appends an event to the backlog and wakes the pump. It never blocks
// and never drops.
func (sub *watchSub) enqueue(ev WatchEvent) {
	sub.mu.Lock()
	sub.buf = append(sub.buf, ev)
	n := len(sub.buf)
	warn := n >= watchBacklogWarn && !sub.warned
	if warn {
		sub.warned = true
	} else if n < watchBacklogWarn {
		sub.warned = false // reset so a later spike warns again
	}
	sub.mu.Unlock()

	if warn {
		sub.logger.Warn("Watch subscriber backlog is large; consumer may be slow",
			log.Str("key", sub.key), log.Int("backlog", n))
	}

	// Signal the pump. The buffer of 1 coalesces bursts: if a wake is already
	// pending the pump will re-check buf anyway, so we can skip.
	select {
	case sub.notify <- struct{}{}:
	default:
	}
}

// pump delivers backlog events to out in order, blocking on a slow consumer
// without affecting anyone else. It exits (closing out) when stop() is called.
func (sub *watchSub) pump() {
	defer close(sub.out)
	for {
		sub.mu.Lock()
		if len(sub.buf) == 0 {
			sub.mu.Unlock()
			select {
			case <-sub.notify:
				continue
			case <-sub.done:
				return
			}
		}
		ev := sub.buf[0]
		if len(sub.buf) == 1 {
			sub.buf = nil // release the backing array in steady state
		} else {
			sub.buf[0] = WatchEvent{} // avoid retaining the delivered event
			sub.buf = sub.buf[1:]
		}
		sub.mu.Unlock()

		select {
		case sub.out <- ev:
		case <-sub.done:
			return
		}
	}
}

// stop terminates the pump. Idempotent and safe to call concurrently.
func (sub *watchSub) stop() {
	sub.stopOnce.Do(func() { close(sub.done) })
}

// Watch sets up a watch for changes to resources of a given type.
func (s *BadgerStore) Watch(ctx context.Context, resourceType types.ResourceType, namespace string) (<-chan WatchEvent, error) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()

	// Check if watchConns is nil (store might be closed)
	if s.watchConns == nil {
		return nil, fmt.Errorf("store is closed, cannot create new watch")
	}

	key := fmt.Sprintf("%s:%s", resourceType, namespace)
	sub := newWatchSub(s.logger, key)
	s.watchConns[key] = append(s.watchConns[key], sub)

	// Remove from active watches when the caller's context is done.
	go func() {
		<-ctx.Done()
		s.watchMu.Lock()
		if s.watchConns != nil {
			conns := s.watchConns[key]
			for i, c := range conns {
				if c == sub {
					s.watchConns[key] = append(conns[:i], conns[i+1:]...)
					break
				}
			}
			if len(s.watchConns[key]) == 0 {
				delete(s.watchConns, key)
			}
		}
		s.watchMu.Unlock()
		sub.stop() // closes out
	}()

	return sub.out, nil
}

// emitWatchEvent delivers a watch event to every matching subscriber. Delivery
// is non-lossy: each subscriber enqueues into its own unbounded backlog, so
// this never blocks and never drops, regardless of consumer speed.
func (s *BadgerStore) emitWatchEvent(eventType WatchEventType, resourceType types.ResourceType, namespace string, name string, resource interface{}, source EventSource) {
	event := WatchEvent{
		Type:         eventType,
		ResourceType: resourceType,
		Namespace:    namespace,
		Name:         name,
		Resource:     resource,
		Source:       source,
	}

	s.watchMu.RLock()
	defer s.watchMu.RUnlock()
	if s.watchConns == nil {
		return
	}

	// Fan out to the four subscription scopes: exact type+namespace, all types
	// in this namespace, this type in all namespaces, and everything.
	s.dispatch(event, fmt.Sprintf("%s:%s", resourceType, namespace))
	s.dispatch(event, fmt.Sprintf(":%s", namespace))
	s.dispatch(event, fmt.Sprintf("%s:", resourceType))
	s.dispatch(event, ":")
}

// dispatch enqueues an event to every subscriber registered under key. The
// caller must hold s.watchMu (read lock is sufficient).
func (s *BadgerStore) dispatch(event WatchEvent, key string) {
	for _, sub := range s.watchConns[key] {
		sub.enqueue(event)
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
