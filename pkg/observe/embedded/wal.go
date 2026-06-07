package embedded

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
)

// The WAL persists the embedded store to node-local disk (under the runed data
// dir) so logs survive a runed restart. It is intentionally simple: an
// append-only set of newline-delimited JSON segment files. The in-memory ring
// stays the query layer; the WAL is durability + crash recovery.
//
// Layout: <dir>/seg-<unixnano>.jsonl. Each runed run opens a fresh active
// segment and seals the previous run's segments (which are replayed into the
// ring on startup). Segments rotate at maxSegBytes; old segments are pruned by
// the store's retention sweep and a hard total-size cap so the WAL can never
// fill the host disk.
const (
	defaultMaxSegBytes   = 16 << 20  // rotate a segment past 16 MiB
	defaultMaxTotalBytes = 256 << 20 // hard cap on WAL disk usage (drop-oldest)
	segPrefix            = "seg-"
	segSuffix            = ".jsonl"
)

type wal struct {
	dir           string
	maxSegBytes   int64
	maxTotalBytes int64
	log           log.Logger

	mu       sync.Mutex
	cur      *os.File
	curW     *bufio.Writer
	curBytes int64
	curPath  string
}

// openWAL prepares the WAL directory and opens a fresh active segment for this
// run. Existing segments are left in place for replay() + prune().
func openWAL(dir string, maxSegBytes, maxTotalBytes int64, logger log.Logger) (*wal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal mkdir: %w", err)
	}
	if maxSegBytes <= 0 {
		maxSegBytes = defaultMaxSegBytes
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = defaultMaxTotalBytes
	}
	w := &wal{dir: dir, maxSegBytes: maxSegBytes, maxTotalBytes: maxTotalBytes, log: logger}
	if err := w.openSegment(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *wal) openSegment() error {
	path := filepath.Join(w.dir, fmt.Sprintf("%s%020d%s", segPrefix, time.Now().UnixNano(), segSuffix))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("wal open segment: %w", err)
	}
	w.cur = f
	w.curW = bufio.NewWriterSize(f, 64<<10)
	w.curBytes = 0
	w.curPath = path
	return nil
}

// append writes a batch as JSON lines to the active segment, rotating when the
// segment grows past maxSegBytes. Buffered — durability is bounded by flush().
func (w *wal) append(records []observe.LogRecord) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range records {
		b, err := json.Marshal(records[i])
		if err != nil {
			continue // skip an unmarshalable record rather than poison the batch
		}
		n, _ := w.curW.Write(b)
		w.curW.WriteByte('\n')
		w.curBytes += int64(n) + 1
	}
	if w.curBytes >= w.maxSegBytes {
		w.rotateLocked()
	}
}

func (w *wal) rotateLocked() {
	if w.curW != nil {
		_ = w.curW.Flush()
	}
	if w.cur != nil {
		_ = w.cur.Sync()
		_ = w.cur.Close()
	}
	if err := w.openSegment(); err != nil {
		w.log.Warn("wal: failed to rotate segment", log.Err(err))
	}
}

// flush persists buffered writes to disk. Called on the sweep tick and Close.
func (w *wal) flush() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.curW != nil {
		_ = w.curW.Flush()
	}
	if w.cur != nil {
		_ = w.cur.Sync()
	}
}

func (w *wal) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.curW != nil {
		_ = w.curW.Flush()
		w.curW = nil
	}
	if w.cur != nil {
		_ = w.cur.Sync()
		_ = w.cur.Close()
		w.cur = nil
	}
}

// replay reconstructs the most-recent up-to-max records (within retention) from
// existing segments, oldest→newest, to seed the ring on startup. Reads
// newest-segment-first and stops once max records are collected, so replay
// memory is bounded by max rather than total disk.
func (w *wal) replay(retention time.Duration, max int, now time.Time) []observe.LogRecord {
	if w == nil {
		return nil
	}
	var cutoff time.Time
	if retention > 0 {
		cutoff = now.Add(-retention)
	}
	segs := w.segments() // ascending (oldest first)
	// Walk newest-first, collecting per-segment record slices until we have max.
	var chunks [][]observe.LogRecord
	total := 0
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] == w.curPath {
			continue // the freshly-opened active segment is empty
		}
		recs := readSegment(segs[i], cutoff)
		if len(recs) == 0 {
			continue
		}
		chunks = append(chunks, recs)
		total += len(recs)
		if max > 0 && total >= max {
			break
		}
	}
	// chunks are newest-segment-first; emit oldest→newest.
	out := make([]observe.LogRecord, 0, total)
	for i := len(chunks) - 1; i >= 0; i-- {
		out = append(out, chunks[i]...)
	}
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// prune enforces retention (delete sealed segments older than the cutoff) and
// the hard total-size cap (drop oldest sealed segments). Never deletes the
// active segment.
func (w *wal) prune(retention time.Duration, now time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	active := w.curPath
	w.mu.Unlock()

	segs := w.segments() // oldest first
	var cutoff time.Time
	if retention > 0 {
		cutoff = now.Add(-retention)
	}

	type segInfo struct {
		path string
		size int64
	}
	var kept []segInfo
	for _, p := range segs {
		if p == active {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		// Retention: a segment whose last write predates the cutoff holds only
		// expired records — drop it.
		if retention > 0 && fi.ModTime().Before(cutoff) {
			if err := os.Remove(p); err != nil {
				w.log.Debug("wal: retention remove failed", log.Str("seg", p), log.Err(err))
			}
			continue
		}
		kept = append(kept, segInfo{path: p, size: fi.Size()})
	}

	// Size cap: drop oldest sealed segments until under the limit. The active
	// segment's bytes count toward the budget.
	var total int64
	for _, s := range kept {
		total += s.size
	}
	w.mu.Lock()
	total += w.curBytes
	w.mu.Unlock()
	for i := 0; i < len(kept) && total > w.maxTotalBytes; i++ {
		if err := os.Remove(kept[i].path); err == nil {
			total -= kept[i].size
		}
	}
}

// segments returns the segment file paths sorted ascending (oldest first). The
// unixnano-padded filenames sort lexicographically by age.
func (w *wal) segments() []string {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, segPrefix) && strings.HasSuffix(n, segSuffix) {
			out = append(out, filepath.Join(w.dir, n))
		}
	}
	sort.Strings(out)
	return out
}

// readSegment parses a segment's JSON lines into records (oldest→newest),
// skipping any record older than cutoff. Tolerant of a truncated trailing line
// from an unclean shutdown.
func readSegment(path string, cutoff time.Time) []observe.LogRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20) // tolerate long log lines
	var out []observe.LogRecord
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r observe.LogRecord
		if err := json.Unmarshal(line, &r); err != nil {
			continue // skip a corrupt/partial line (e.g. crash mid-write)
		}
		if !cutoff.IsZero() && r.Timestamp.Before(cutoff) {
			continue
		}
		out = append(out, r)
	}
	return out
}
