package observe

import "time"

// StoreStats are store-level facts for the dashboard's Sources page: how much
// is stored, how much disk it uses, and the store's bounds. Only stores that
// own their storage (the embedded store) can report them; external backends
// (Loki, ClickHouse) manage their own storage and report Supported=false.
type StoreStats struct {
	// Records currently held.
	Records int64

	// DiskUsedBytes is the on-disk footprint (0 for a purely in-memory store).
	DiskUsedBytes int64

	// DiskCapBytes is the hard disk bound (drop-oldest), 0 if unbounded.
	DiskCapBytes int64

	// Retention is the store's age bound (0 = store default semantics,
	// negative = age-based eviction disabled).
	Retention time.Duration

	// OldestRecord is the timestamp of the oldest held record (zero when
	// empty) — i.e. how far back queries can actually see.
	OldestRecord time.Time
}

// StatsProvider is an optional interface a LogStore may implement to report
// StoreStats. The ObserveService type-asserts for it; stores that don't
// implement it surface as "managed by the backend" in the dashboard.
type StatsProvider interface {
	Stats() StoreStats
}
