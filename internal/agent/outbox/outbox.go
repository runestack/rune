// Package outbox is the agent's per-node buffer for logs and events
// destined for remote sinks (RuneSight, CloudWatch, Datadog).
//
// The outbox is intentionally simple in RUNE-032: a bounded in-memory
// channel with a drop-oldest policy on overflow. Persistence and
// pluggable sinks land with the observability work in Release 4. The
// abstraction exists now so subsystems built in the next networking
// tickets (data plane, DNS, policy) write to a stable interface that
// will not change when the real outbox lands.
package outbox

import (
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
)

// Kind classifies an outbox entry.
type Kind uint8

const (
	// KindLog is a log line emitted by an agent subsystem.
	KindLog Kind = iota + 1
	// KindEvent is a structured event (policy drop, endpoint change,
	// ingress lifecycle, etc.).
	KindEvent
)

// Entry is a single buffered item.
type Entry struct {
	Kind      Kind
	Timestamp time.Time
	Source    string                 // subsystem name
	Message   string                 // for KindLog; short summary for KindEvent
	Fields    map[string]interface{} // structured fields
}

// Outbox is a bounded, thread-safe FIFO of Entries. Producers call
// Push; consumers (sink writers) call Drain in batches.
type Outbox struct {
	cap int
	log log.Logger

	mu      sync.Mutex
	buf     []Entry
	dropped uint64
	closed  bool
}

// New constructs an Outbox with the given capacity. capacity <= 0
// defaults to 1024.
func New(capacity int, logger log.Logger) *Outbox {
	if capacity <= 0 {
		capacity = 1024
	}
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("outbox")
	}
	return &Outbox{
		cap: capacity,
		log: logger,
		buf: make([]Entry, 0, capacity),
	}
}

// Push adds e to the outbox. If the outbox is full, the oldest entry
// is evicted and a drop counter is incremented. Push never blocks.
//
// Push on a closed Outbox is a no-op.
func (o *Outbox) Push(e Entry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if len(o.buf) >= o.cap {
		// Drop oldest. We accept the cost of the slice shift here in
		// exchange for a trivial implementation; the outbox is meant
		// to hold seconds of buffer, not a deep queue.
		o.buf = o.buf[1:]
		o.dropped++
		// Log drops at most once per 1000 drops to avoid log floods.
		if o.dropped == 1 || o.dropped%1000 == 0 {
			o.log.Warn("outbox dropping entries",
				log.Int("capacity", o.cap),
				log.Int64("dropped_total", int64(o.dropped)),
			)
		}
	}
	o.buf = append(o.buf, e)
}

// Drain removes up to max entries (oldest first) and returns them. If
// the outbox is empty, Drain returns nil. max <= 0 drains everything.
func (o *Outbox) Drain(max int) []Entry {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.buf) == 0 {
		return nil
	}
	n := len(o.buf)
	if max > 0 && max < n {
		n = max
	}
	out := make([]Entry, n)
	copy(out, o.buf[:n])
	o.buf = o.buf[n:]
	return out
}

// Len returns the current buffer depth. Useful for metrics.
func (o *Outbox) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.buf)
}

// Dropped returns the cumulative count of evicted entries.
func (o *Outbox) Dropped() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dropped
}

// Close marks the outbox as closed. Subsequent Pushes are dropped
// silently. Drain still returns any entries already buffered.
func (o *Outbox) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
}
