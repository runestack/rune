package embedded

import (
	"os"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

var zeroTime time.Time

// Compile-time assertion: the embedded store reports stats.
var _ observe.StatsProvider = (*Store)(nil)

// Stats reports the store's current footprint for the dashboard Sources page.
func (s *Store) Stats() observe.StoreStats {
	s.mu.RLock()
	records := int64(len(s.buf))
	var oldest = zeroTime
	if len(s.buf) > 0 {
		oldest = s.buf[0].Timestamp
	}
	s.mu.RUnlock()

	st := observe.StoreStats{
		Records:      records,
		Retention:    s.retention,
		OldestRecord: oldest,
	}
	if s.wal != nil {
		st.DiskUsedBytes = s.wal.diskUsage()
		st.DiskCapBytes = s.wal.maxTotalBytes
	}
	return st
}

// diskUsage sums the WAL's segment file sizes (best-effort; nil-safe).
func (w *wal) diskUsage() int64 {
	if w == nil {
		return 0
	}
	var total int64
	for _, p := range w.segments() {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}
