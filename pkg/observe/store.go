package observe

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented is returned by store methods that are scaffolded but not
// yet wired to a backend (the Loki / ClickHouse skeletons). Callers (and the
// conformance suite) can use errors.Is to detect a stubbed path.
var ErrNotImplemented = errors.New("observe: not implemented")

// ErrCapabilityUnsupported is returned when a Query asks for something the
// store does not advertise in Capabilities (e.g. RawSQL against the embedded
// store or Loki).
var ErrCapabilityUnsupported = errors.New("observe: capability not supported by backend")

// LogStore is the single seam between the observability control plane and its
// backend. Implementations live under embedded/ (the default), loki/, and
// clickhouse/. See plan §4.
type LogStore interface {
	// Write persists a batch of enriched records (the ingest path). Stores
	// translate to the backend's native write. Must be safe for concurrent
	// callers.
	Write(ctx context.Context, batch []LogRecord) error

	// Execute runs a parsed Query and streams results. It receives an AST,
	// never raw LogQL text. Stores reject queries that exceed their
	// Capabilities with ErrCapabilityUnsupported.
	Execute(ctx context.Context, q *Query) (ResultStream, error)

	// Capabilities reports the store's capability handshake for dashboard
	// feature-flagging. Cheap and side-effect free.
	Capabilities() Capabilities

	// Labels returns known values for the dimensions named by sel — used to
	// populate label chips and autocomplete in the dashboard.
	Labels(ctx context.Context, sel Selector) ([]LabelValue, error)

	// Health is a liveness/readiness probe against the backend.
	Health(ctx context.Context) error
}

// Selector narrows a Labels lookup.
type Selector struct {
	// Name is the dimension whose values to enumerate (e.g. "service"). Empty
	// means "enumerate the available label names rather than values".
	Name string

	// Match optionally constrains which streams contribute values (e.g. only
	// services within a namespace). Empty means all streams.
	Match []Matcher

	// Start and End bound the time window considered. Zero values mean the
	// store's default lookback.
	Start time.Time
	End   time.Time

	// Limit caps the number of returned values. Zero means store default.
	Limit int
}

// LabelValue is one value of a label dimension, with an optional occurrence
// count for ranking in the dashboard.
type LabelValue struct {
	Name  string
	Value string
	Count int64
}

// ResultStream is the result of Execute. Callers must Close it. A single
// Execute yields either log rows (raw query) or metric samples (aggregation),
// never both — IsMetric reports which.
type ResultStream interface {
	// IsMetric reports whether Next yields MetricSample (true) or LogRow.
	IsMetric() bool

	// Next advances to the next result. It returns false at end of stream or
	// on error; check Err after Next returns false.
	Next(ctx context.Context) bool

	// Row returns the current log line. Valid only when IsMetric is false and
	// the last Next returned true.
	Row() LogRow

	// Sample returns the current metric sample. Valid only when IsMetric is
	// true and the last Next returned true.
	Sample() MetricSample

	// Err returns the first error encountered, if any.
	Err() error

	// Close releases backend resources. Safe to call multiple times.
	Close() error
}

// LogRow is one returned log line on the query path. It mirrors LogRecord but
// is the read-side shape (a backend may not round-trip every ingest field).
type LogRow struct {
	Timestamp time.Time
	Line      string
	Stream    string
	Level     string
	Labels    map[string]string
}

// MetricSample is one bucket of an aggregation result: a value at a timestamp,
// tagged by the GroupBy dimensions that produced it.
type MetricSample struct {
	// Timestamp is the left edge of the histogram bucket.
	Timestamp time.Time

	// Value is the aggregated value (count, rate, percentile, ...).
	Value float64

	// GroupLabels are the resolved by(...) dimensions for this series
	// (e.g. {"level":"error"} for a level breakdown). nil for ungrouped.
	GroupLabels map[string]string
}
