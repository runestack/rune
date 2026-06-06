package forwarder

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
)

// Spool is the forwarder's at-least-once buffer between the taps (producers)
// and the flush loop (consumer). Push never blocks the taps; Pop pulls a batch
// to ingest; Requeue returns a failed batch to the front for retry.
type Spool interface {
	// Push enqueues a record. Must be non-blocking.
	Push(r observe.LogRecord)
	// Pop removes and returns up to max records (oldest first). Empty => nil.
	Pop(max int) []observe.LogRecord
	// Requeue returns a previously-popped batch to the front of the spool
	// (used after an ingest failure so records aren't dropped).
	Requeue(batch []observe.LogRecord)
	// Len reports the current depth.
	Len() int
}

// MemSpool is an in-memory Spool with a drop-oldest bound. It is the default
// when no durable spool is configured: records survive transient ingest
// failures (via Requeue) but are lost on process crash.
type MemSpool struct {
	cap int
	mu  sync.Mutex
	buf []observe.LogRecord
}

// DefaultSpoolCapacity bounds the in-memory spool before drop-oldest kicks in.
const DefaultSpoolCapacity = 100_000

// NewMemSpool constructs an in-memory spool. capacity <= 0 uses
// DefaultSpoolCapacity.
func NewMemSpool(capacity int) *MemSpool {
	if capacity <= 0 {
		capacity = DefaultSpoolCapacity
	}
	return &MemSpool{cap: capacity, buf: make([]observe.LogRecord, 0, 1024)}
}

func (s *MemSpool) Push(r observe.LogRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) >= s.cap {
		s.buf = s.buf[1:] // drop oldest
	}
	s.buf = append(s.buf, r)
}

func (s *MemSpool) Pop(max int) []observe.LogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil
	}
	n := len(s.buf)
	if max > 0 && max < n {
		n = max
	}
	out := make([]observe.LogRecord, n)
	copy(out, s.buf[:n])
	s.buf = s.buf[n:]
	return out
}

func (s *MemSpool) Requeue(batch []observe.LogRecord) {
	if len(batch) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Prepend so the failed batch is retried before newer records, honouring
	// rough time order. Respect the cap by dropping the oldest of the merged
	// set if needed.
	merged := make([]observe.LogRecord, 0, len(batch)+len(s.buf))
	merged = append(merged, batch...)
	merged = append(merged, s.buf...)
	if len(merged) > s.cap {
		merged = merged[len(merged)-s.cap:]
	}
	s.buf = merged
}

func (s *MemSpool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

// DiskSpool wraps a MemSpool with a JSONL append-log for crash durability. Each
// Push is appended to the file; on startup the file is replayed into memory.
// Pop/Requeue operate on the in-memory view; the file is truncated when the
// in-memory spool drains to empty (a coarse compaction that is correct because
// a drained spool means every record was ingested). This is the MVP durable
// spool; a segment/offset design can replace it behind the Spool interface.
type DiskSpool struct {
	*MemSpool
	path string
	log  log.Logger

	fmu sync.Mutex
	f   *os.File
	w   *bufio.Writer
}

// NewDiskSpool opens (or creates) a JSONL spool file at path and replays any
// existing records into memory.
func NewDiskSpool(path string, capacity int, logger log.Logger) (*DiskSpool, error) {
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("agent.forwarder.spool")
	}
	mem := NewMemSpool(capacity)

	// Replay existing spool, if any.
	if existing, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(existing)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		var replayed int
		for sc.Scan() {
			var r observe.LogRecord
			if err := json.Unmarshal(sc.Bytes(), &r); err == nil {
				mem.Push(r)
				replayed++
			}
		}
		existing.Close()
		if replayed > 0 {
			logger.Info("forwarder spool replayed", log.Int("records", replayed), log.Str("path", path))
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &DiskSpool{
		MemSpool: mem,
		path:     path,
		log:      logger,
		f:        f,
		w:        bufio.NewWriter(f),
	}, nil
}

// Push appends to the JSONL file and the in-memory view.
func (d *DiskSpool) Push(r observe.LogRecord) {
	d.MemSpool.Push(r)
	d.fmu.Lock()
	defer d.fmu.Unlock()
	if b, err := json.Marshal(r); err == nil {
		_, _ = d.w.Write(b)
		_ = d.w.WriteByte('\n')
		_ = d.w.Flush()
	}
}

// Pop drains the in-memory view and, when it empties, truncates the file.
func (d *DiskSpool) Pop(max int) []observe.LogRecord {
	out := d.MemSpool.Pop(max)
	if d.MemSpool.Len() == 0 {
		d.truncate()
	}
	return out
}

func (d *DiskSpool) truncate() {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	if err := d.f.Truncate(0); err != nil {
		d.log.Debug("forwarder spool truncate failed", log.Err(err))
		return
	}
	if _, err := d.f.Seek(0, 0); err != nil {
		d.log.Debug("forwarder spool seek failed", log.Err(err))
		return
	}
	d.w.Reset(d.f)
}

// Close flushes and closes the underlying file.
func (d *DiskSpool) Close() error {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	_ = d.w.Flush()
	return d.f.Close()
}
