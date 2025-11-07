package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"encoding/json"

	"github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/worker"
)

var (
	ErrTaskNotFound = errors.New("task not found")
)

// TaskStore defines the interface for storing and retrieving tasks with their execution state
type TaskStore interface {
	// Save stores a task (including its execution state and results)
	Save(ctx context.Context, task worker.Task) error

	// Get retrieves a task by ID
	Get(ctx context.Context, id string) (worker.Task, error)

	// List retrieves tasks matching the filter
	List(ctx context.Context, filter worker.TaskFilter) ([]worker.Task, error)

	// Update updates an existing task (including execution state)
	Update(ctx context.Context, task worker.Task) error

	// Delete removes a task
	Delete(ctx context.Context, id string) error

	// GetByStatus retrieves tasks by status (convenience method)
	GetByStatus(ctx context.Context, status worker.TaskStatus) ([]worker.Task, error)

	// GetCompleted retrieves all completed tasks
	GetCompleted(ctx context.Context) ([]worker.Task, error)

	// GetFailed retrieves all failed tasks
	GetFailed(ctx context.Context) ([]worker.Task, error)
}

// MemoryTaskStore implements TaskStore using in-memory storage
type MemoryTaskStore struct {
	mu    sync.RWMutex
	tasks map[string]worker.Task
}

// NewMemoryTaskStore creates a new memory task store
func NewMemoryTaskStore() *MemoryTaskStore {
	return &MemoryTaskStore{
		tasks: make(map[string]worker.Task),
	}
}

func (s *MemoryTaskStore) Save(ctx context.Context, task worker.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.GetID()] = task
	return nil
}

func (s *MemoryTaskStore) Get(ctx context.Context, id string) (worker.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return task, nil
}

func (s *MemoryTaskStore) List(ctx context.Context, filter worker.TaskFilter) ([]worker.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var tasks []worker.Task
	for _, task := range s.tasks {
		if matchesFilter(task, filter) {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

func (s *MemoryTaskStore) Update(ctx context.Context, task worker.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[task.GetID()]; !ok {
		return ErrTaskNotFound
	}
	s.tasks[task.GetID()] = task
	return nil
}

func (s *MemoryTaskStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[id]; !ok {
		return ErrTaskNotFound
	}
	delete(s.tasks, id)
	return nil
}

// matchesFilter checks if a task matches the given filter
func matchesFilter(task worker.Task, filter worker.TaskFilter) bool {
	if filter.Type != "" && task.GetType() != filter.Type {
		return false
	}
	if filter.Status != "" && task.GetStatus() != filter.Status {
		return false
	}
	if !filter.StartTime.IsZero() && task.GetStartTime().Before(filter.StartTime) {
		return false
	}
	if !filter.EndTime.IsZero() && task.GetEndTime().After(filter.EndTime) {
		return false
	}
	return true
}

// Convenience method implementations for MemoryTaskStore
func (s *MemoryTaskStore) GetByStatus(ctx context.Context, status worker.TaskStatus) ([]worker.Task, error) {
	return s.List(ctx, worker.TaskFilter{Status: status})
}

func (s *MemoryTaskStore) GetCompleted(ctx context.Context) ([]worker.Task, error) {
	return s.GetByStatus(ctx, worker.TaskStatusCompleted)
}

func (s *MemoryTaskStore) GetFailed(ctx context.Context) ([]worker.Task, error) {
	return s.GetByStatus(ctx, worker.TaskStatusFailed)
}

// PersistentTaskStore implements TaskStore with persistence
type PersistentTaskStore struct {
	store TaskStore
}

// NewPersistentTaskStore creates a new persistent task store
func NewPersistentTaskStore(store TaskStore) *PersistentTaskStore {
	return &PersistentTaskStore{
		store: store,
	}
}

func (s *PersistentTaskStore) Save(ctx context.Context, task worker.Task) error {
	return s.store.Save(ctx, task)
}

func (s *PersistentTaskStore) Get(ctx context.Context, id string) (worker.Task, error) {
	return s.store.Get(ctx, id)
}

func (s *PersistentTaskStore) List(ctx context.Context, filter worker.TaskFilter) ([]worker.Task, error) {
	return s.store.List(ctx, filter)
}

func (s *PersistentTaskStore) Update(ctx context.Context, task worker.Task) error {
	return s.store.Update(ctx, task)
}

func (s *PersistentTaskStore) Delete(ctx context.Context, id string) error {
	return s.store.Delete(ctx, id)
}

// Convenience method implementations for PersistentTaskStore
func (s *PersistentTaskStore) GetByStatus(ctx context.Context, status worker.TaskStatus) ([]worker.Task, error) {
	return s.store.GetByStatus(ctx, status)
}

func (s *PersistentTaskStore) GetCompleted(ctx context.Context) ([]worker.Task, error) {
	return s.store.GetCompleted(ctx)
}

func (s *PersistentTaskStore) GetFailed(ctx context.Context) ([]worker.Task, error) {
	return s.store.GetFailed(ctx)
}

// BadgerTaskStore implements TaskStore using BadgerDB for persistence
type BadgerTaskStore struct {
	db *badger.DB
}

// NewBadgerTaskStore creates a new BadgerDB-based task store
func NewBadgerTaskStore(dbPath string) (*BadgerTaskStore, error) {
	opts := badger.DefaultOptions(dbPath)
	opts.Logger = nil // Disable badger logging

	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to open BadgerDB: %w", err)
	}

	return &BadgerTaskStore{
		db: db,
	}, nil
}

// Close closes the BadgerDB connection
func (s *BadgerTaskStore) Close() error {
	return s.db.Close()
}

func (s *BadgerTaskStore) Save(ctx context.Context, task worker.Task) error {
	// Serialize task to JSON
	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}

	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte("task:" + task.GetID())
		return txn.Set(key, data)
	})
}

func (s *BadgerTaskStore) Get(ctx context.Context, id string) (worker.Task, error) {
	var taskData []byte

	err := s.db.View(func(txn *badger.Txn) error {
		key := []byte("task:" + id)
		item, err := txn.Get(key)
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			taskData = append([]byte(nil), val...)
			return nil
		})
	})

	if err != nil {
		if err == badger.ErrKeyNotFound {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("failed to get task: %w", err)
	}

	// For this implementation, we'll need to deserialize to a concrete type
	// In a full implementation, you'd have a task registry or use interface serialization
	var baseTask worker.BaseTask
	if err := json.Unmarshal(taskData, &baseTask); err != nil {
		return nil, fmt.Errorf("failed to unmarshal task: %w", err)
	}

	return &baseTask, nil
}

func (s *BadgerTaskStore) List(ctx context.Context, filter worker.TaskFilter) ([]worker.Task, error) {
	var tasks []worker.Task

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte("task:")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()

			err := item.Value(func(val []byte) error {
				var baseTask worker.BaseTask
				if err := json.Unmarshal(val, &baseTask); err != nil {
					return err
				}

				if matchesFilter(&baseTask, filter) {
					tasks = append(tasks, &baseTask)
				}
				return nil
			})

			if err != nil {
				return err
			}
		}
		return nil
	})

	return tasks, err
}

func (s *BadgerTaskStore) Update(ctx context.Context, task worker.Task) error {
	// Check if task exists first
	_, err := s.Get(ctx, task.GetID())
	if err != nil {
		return err
	}

	// Update is the same as save for BadgerDB
	return s.Save(ctx, task)
}

func (s *BadgerTaskStore) Delete(ctx context.Context, id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte("task:" + id)
		return txn.Delete(key)
	})
}

// Convenience method implementations for BadgerTaskStore
func (s *BadgerTaskStore) GetByStatus(ctx context.Context, status worker.TaskStatus) ([]worker.Task, error) {
	return s.List(ctx, worker.TaskFilter{Status: status})
}

func (s *BadgerTaskStore) GetCompleted(ctx context.Context) ([]worker.Task, error) {
	return s.GetByStatus(ctx, worker.TaskStatusCompleted)
}

func (s *BadgerTaskStore) GetFailed(ctx context.Context) ([]worker.Task, error) {
	return s.GetByStatus(ctx, worker.TaskStatusFailed)
}
