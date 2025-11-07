package queue

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/worker"
)

// QueueType represents the type of queue
type QueueType string

const (
	QueueTypePriority   QueueType = "priority"
	QueueTypeDelayed    QueueType = "delayed"
	QueueTypeDeadLetter QueueType = "dead_letter"
	QueueTypeRateLimit  QueueType = "rate_limit"
	QueueTypeBatch      QueueType = "batch"
	QueueTypeFIFO       QueueType = "fifo"
)

// Queue defines the interface for task queues
type Queue interface {
	// Add adds a task to the queue
	Add(ctx context.Context, task worker.Task) error

	// Get retrieves the next task from the queue
	Get(ctx context.Context) (worker.Task, error)

	// Remove removes a task from the queue
	Remove(ctx context.Context, taskID string) error

	// Size returns the number of tasks in the queue
	Size(ctx context.Context) (int, error)

	// Clear removes all tasks from the queue
	Clear(ctx context.Context) error
}

// PriorityQueue implements a priority queue for tasks
type priorityQueue struct {
	items    []worker.Task
	mu       sync.RWMutex
	capacity int
}

// heap.Interface implementation for priorityQueue
func (pq *priorityQueue) Len() int { return len(pq.items) }

func (pq *priorityQueue) Less(i, j int) bool {
	return pq.items[i].GetPriority() > pq.items[j].GetPriority()
}

func (pq *priorityQueue) Swap(i, j int) {
	pq.items[i], pq.items[j] = pq.items[j], pq.items[i]
}

func (pq *priorityQueue) Push(x interface{}) {
	pq.items = append(pq.items, x.(worker.Task))
}

func (pq *priorityQueue) Pop() interface{} {
	n := len(pq.items)
	item := pq.items[n-1]
	pq.items = pq.items[:n-1]
	return item
}

// NewPriorityQueue creates a new priority queue
func NewPriorityQueue(capacity int) *priorityQueue {
	pq := &priorityQueue{
		items:    make([]worker.Task, 0),
		capacity: capacity,
	}
	heap.Init(pq)
	return pq
}

func (pq *priorityQueue) Add(ctx context.Context, task worker.Task) error {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	if pq.capacity > 0 && len(pq.items) >= pq.capacity {
		return ErrQueueFull
	}

	heap.Push(pq, task)
	return nil
}

func (pq *priorityQueue) Get(ctx context.Context) (worker.Task, error) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	if len(pq.items) == 0 {
		return nil, ErrQueueEmpty
	}

	return heap.Pop(pq).(worker.Task), nil
}

func (pq *priorityQueue) Remove(ctx context.Context, taskID string) error {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	for i, task := range pq.items {
		if task.GetID() == taskID {
			heap.Remove(pq, i)
			return nil
		}
	}
	return ErrTaskNotFound
}

func (pq *priorityQueue) Size(ctx context.Context) (int, error) {
	pq.mu.RLock()
	defer pq.mu.RUnlock()
	return len(pq.items), nil
}

func (pq *priorityQueue) Clear(ctx context.Context) error {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	pq.items = make([]worker.Task, 0)
	return nil
}

// DelayedQueue implements a queue for delayed task execution
type delayedQueue struct {
	items    []*delayedItem
	mu       sync.RWMutex
	capacity int
}

// heap.Interface implementation for delayedQueue
func (dq *delayedQueue) Len() int { return len(dq.items) }

func (dq *delayedQueue) Less(i, j int) bool {
	return dq.items[i].executeAt.Before(dq.items[j].executeAt)
}

func (dq *delayedQueue) Swap(i, j int) {
	dq.items[i], dq.items[j] = dq.items[j], dq.items[i]
	dq.items[i].index = i
	dq.items[j].index = j
}

func (dq *delayedQueue) Push(x interface{}) {
	n := len(dq.items)
	item := x.(*delayedItem)
	item.index = n
	dq.items = append(dq.items, item)
}

func (dq *delayedQueue) Pop() interface{} {
	n := len(dq.items)
	item := dq.items[n-1]
	dq.items = dq.items[:n-1]
	return item
}

type delayedItem struct {
	task      worker.Task
	executeAt time.Time
	index     int
}

// NewDelayedQueue creates a new delayed queue
func NewDelayedQueue(capacity int) *delayedQueue {
	dq := &delayedQueue{
		items:    make([]*delayedItem, 0),
		capacity: capacity,
	}
	heap.Init(dq)
	return dq
}

func (dq *delayedQueue) Add(ctx context.Context, task worker.Task) error {
	return dq.PushAfter(ctx, task, task.GetTimeout())
}

func (dq *delayedQueue) PushAfter(ctx context.Context, task worker.Task, delay time.Duration) error {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	if dq.capacity > 0 && len(dq.items) >= dq.capacity {
		return ErrQueueFull
	}

	item := &delayedItem{
		task:      task,
		executeAt: time.Now().Add(delay),
	}
	heap.Push(dq, item)
	return nil
}

func (dq *delayedQueue) Get(ctx context.Context) (worker.Task, error) {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	if len(dq.items) == 0 {
		return nil, ErrQueueEmpty
	}

	item := heap.Pop(dq).(*delayedItem)
	if time.Now().Before(item.executeAt) {
		heap.Push(dq, item)
		return nil, ErrTaskNotReady
	}

	return item.task, nil
}

func (dq *delayedQueue) Remove(ctx context.Context, taskID string) error {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	for i, item := range dq.items {
		if item.task.GetID() == taskID {
			heap.Remove(dq, i)
			return nil
		}
	}
	return ErrTaskNotFound
}

func (dq *delayedQueue) Size(ctx context.Context) (int, error) {
	dq.mu.RLock()
	defer dq.mu.RUnlock()
	return len(dq.items), nil
}

func (dq *delayedQueue) Clear(ctx context.Context) error {
	dq.mu.Lock()
	defer dq.mu.Unlock()
	dq.items = make([]*delayedItem, 0)
	return nil
}

// RateLimitQueue implements a rate-limited queue
type rateLimitQueue struct {
	queue    Queue
	rate     time.Duration
	lastExec time.Time
	mu       sync.Mutex
	capacity int
}

// NewRateLimitQueue creates a new rate-limited queue
func NewRateLimitQueue(queue Queue, rate time.Duration, capacity int) *rateLimitQueue {
	return &rateLimitQueue{
		queue:    queue,
		rate:     rate,
		lastExec: time.Now(),
		capacity: capacity,
	}
}

func (rq *rateLimitQueue) Add(ctx context.Context, task worker.Task) error {
	return rq.queue.Add(ctx, task)
}

func (rq *rateLimitQueue) Get(ctx context.Context) (worker.Task, error) {
	rq.mu.Lock()
	defer rq.mu.Unlock()

	// Check if we need to wait before getting the next task
	now := time.Now()
	timeSinceLastExec := now.Sub(rq.lastExec)
	if timeSinceLastExec < rq.rate {
		// Wait for the remaining time
		time.Sleep(rq.rate - timeSinceLastExec)
	}

	// Get the next task
	task, err := rq.queue.Get(ctx)
	if err != nil {
		return nil, err
	}

	// Update last execution time
	rq.lastExec = time.Now()
	return task, nil
}

func (rq *rateLimitQueue) Remove(ctx context.Context, taskID string) error {
	return rq.queue.Remove(ctx, taskID)
}

func (rq *rateLimitQueue) Size(ctx context.Context) (int, error) {
	return rq.queue.Size(ctx)
}

func (rq *rateLimitQueue) Clear(ctx context.Context) error {
	return rq.queue.Clear(ctx)
}

// BatchQueue implements a queue that processes tasks in batches
type batchQueue struct {
	queue     Queue
	batchSize int
	mu        sync.Mutex
	capacity  int
}

// NewBatchQueue creates a new batch queue
func NewBatchQueue(queue Queue, batchSize, capacity int) *batchQueue {
	return &batchQueue{
		queue:     queue,
		batchSize: batchSize,
		capacity:  capacity,
	}
}

func (bq *batchQueue) Add(ctx context.Context, task worker.Task) error {
	return bq.queue.Add(ctx, task)
}

func (bq *batchQueue) Get(ctx context.Context) (worker.Task, error) {
	bq.mu.Lock()
	defer bq.mu.Unlock()

	size, err := bq.queue.Size(ctx)
	if err != nil {
		return nil, err
	}

	if size < bq.batchSize {
		return nil, ErrBatchNotReady
	}

	return bq.queue.Get(ctx)
}

func (bq *batchQueue) PopBatch(ctx context.Context) ([]worker.Task, error) {
	bq.mu.Lock()
	defer bq.mu.Unlock()

	size, err := bq.queue.Size(ctx)
	if err != nil {
		return nil, err
	}

	if size < bq.batchSize {
		return nil, ErrBatchNotReady
	}

	batch := make([]worker.Task, 0, bq.batchSize)
	for i := 0; i < bq.batchSize; i++ {
		task, err := bq.queue.Get(ctx)
		if err != nil {
			return nil, err
		}
		batch = append(batch, task)
	}

	return batch, nil
}

func (bq *batchQueue) Remove(ctx context.Context, taskID string) error {
	return bq.queue.Remove(ctx, taskID)
}

func (bq *batchQueue) Size(ctx context.Context) (int, error) {
	return bq.queue.Size(ctx)
}

func (bq *batchQueue) Clear(ctx context.Context) error {
	return bq.queue.Clear(ctx)
}

// FIFOQueue implements a first-in-first-out queue for tasks
type fifoQueue struct {
	items    []worker.Task
	mu       sync.RWMutex
	capacity int
}

// NewFIFOQueue creates a new FIFO queue
func NewFIFOQueue(capacity int) *fifoQueue {
	return &fifoQueue{
		items:    make([]worker.Task, 0),
		capacity: capacity,
	}
}

func (fq *fifoQueue) Add(ctx context.Context, task worker.Task) error {
	fq.mu.Lock()
	defer fq.mu.Unlock()

	if fq.capacity > 0 && len(fq.items) >= fq.capacity {
		return ErrQueueFull
	}

	fq.items = append(fq.items, task)
	return nil
}

func (fq *fifoQueue) Get(ctx context.Context) (worker.Task, error) {
	fq.mu.Lock()
	defer fq.mu.Unlock()

	if len(fq.items) == 0 {
		return nil, ErrQueueEmpty
	}

	task := fq.items[0]
	fq.items = fq.items[1:]
	return task, nil
}

func (fq *fifoQueue) Remove(ctx context.Context, taskID string) error {
	fq.mu.Lock()
	defer fq.mu.Unlock()

	for i, task := range fq.items {
		if task.GetID() == taskID {
			fq.items = append(fq.items[:i], fq.items[i+1:]...)
			return nil
		}
	}
	return ErrTaskNotFound
}

func (fq *fifoQueue) Size(ctx context.Context) (int, error) {
	fq.mu.RLock()
	defer fq.mu.RUnlock()
	return len(fq.items), nil
}

func (fq *fifoQueue) Clear(ctx context.Context) error {
	fq.mu.Lock()
	defer fq.mu.Unlock()
	fq.items = make([]worker.Task, 0)
	return nil
}

// DeadLetterQueue implements a queue for permanently failed tasks
type DeadLetterQueue struct {
	tasks    []worker.Task
	capacity int
	mu       sync.RWMutex
}

// NewDeadLetterQueue creates a new dead letter queue
func NewDeadLetterQueue(capacity int) *DeadLetterQueue {
	return &DeadLetterQueue{
		tasks:    make([]worker.Task, 0),
		capacity: capacity,
	}
}

// Add adds a task to the dead letter queue
func (q *DeadLetterQueue) Add(ctx context.Context, task worker.Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.tasks) >= q.capacity {
		return ErrQueueFull
	}

	q.tasks = append(q.tasks, task)
	return nil
}

// Get retrieves a task from the dead letter queue (FIFO)
func (q *DeadLetterQueue) Get(ctx context.Context) (worker.Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.tasks) == 0 {
		return nil, ErrQueueEmpty
	}

	task := q.tasks[0]
	q.tasks = q.tasks[1:]
	return task, nil
}

// Peek returns the next task without removing it
func (q *DeadLetterQueue) Peek(ctx context.Context) (worker.Task, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	if len(q.tasks) == 0 {
		return nil, ErrQueueEmpty
	}

	return q.tasks[0], nil
}

// Remove removes a specific task from the queue
func (q *DeadLetterQueue) Remove(ctx context.Context, taskID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, task := range q.tasks {
		if task.GetID() == taskID {
			q.tasks = append(q.tasks[:i], q.tasks[i+1:]...)
			return nil
		}
	}

	return fmt.Errorf("task not found: %s", taskID)
}

// Size returns the number of tasks in the queue
func (q *DeadLetterQueue) Size(ctx context.Context) (int, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.tasks), nil
}

// Clear removes all tasks from the queue
func (q *DeadLetterQueue) Clear(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tasks = q.tasks[:0]
	return nil
}

// List returns all tasks in the dead letter queue for inspection
func (q *DeadLetterQueue) List(ctx context.Context) ([]worker.Task, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	tasks := make([]worker.Task, len(q.tasks))
	copy(tasks, q.tasks)
	return tasks, nil
}

// Error definitions
var (
	ErrQueueFull         = errors.New("queue is full")
	ErrQueueEmpty        = errors.New("queue is empty")
	ErrTaskNotFound      = errors.New("task not found")
	ErrTaskNotReady      = errors.New("task is not ready for execution")
	ErrRateLimitExceeded = errors.New("rate limit exceeded")
	ErrBatchNotReady     = errors.New("batch is not ready for processing")
)
