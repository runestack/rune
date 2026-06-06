// Package clickhouse implements observe.LogStore against a ClickHouse backend
// (the analytical optional sink). It lowers the Query AST to SQL and writes via
// batched INSERT. It supports both the Core and Advanced capability tiers: raw
// SQL mode, percentiles on arbitrary fields, cross-stream joins, and
// high-cardinality field filters.
//
// Skeleton: the struct, constructor, and Capabilities are real; the I/O bodies
// return observe.ErrNotImplemented pending the adapter port and the LogQL→SQL
// lowering (plan §6.4, §7 step 6). Hot fields (level, status, dur, service,
// instance) are promoted to typed columns; the raw line uses a text skip index.
package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// Compile-time assertion that Store satisfies the seam.
var _ observe.LogStore = (*Store)(nil)

// Config configures the ClickHouse store.
type Config struct {
	// DSN is the ClickHouse connection string
	// (e.g. clickhouse://user:pass@host:9000/runesight).
	DSN string

	// Table is the target MergeTree table for log rows (default "logs").
	Table string

	// Database is the target database (default "runesight").
	Database string

	// DialTimeout bounds connection establishment. Zero uses a default.
	DialTimeout time.Duration
}

// Store is the ClickHouse-backed observe.LogStore.
type Store struct {
	cfg Config
	// TODO: hold the clickhouse-go driver.Conn (or database/sql pool) here once
	// the driver dependency is added. Kept untyped until then so the module
	// stays dependency-light (only add a dep when actually referenced).
}

// New constructs a ClickHouse store. It does not dial; call Health to verify
// connectivity.
func New(cfg Config) (*Store, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("clickhouse: DSN is required")
	}
	if cfg.Table == "" {
		cfg.Table = "logs"
	}
	if cfg.Database == "" {
		cfg.Database = "runesight"
	}
	return &Store{cfg: cfg}, nil
}

// Capabilities reports the ClickHouse capability handshake: Advanced tier
// (which implies Core). Enables the dashboard's SQL mode and advanced widgets.
func (s *Store) Capabilities() observe.Capabilities {
	return observe.Capabilities{
		Backend:                "clickhouse",
		MaxTier:                observe.TierAdvanced,
		RawSQL:                 true,
		Percentiles:            true,
		HighCardinalityFilters: true,
	}
}

// Write persists a batch via a batched INSERT.
//
// TODO: open a batch against `INSERT INTO <db>.<table> (timestamp, namespace,
// service, instance, node, level, stream, line, labels)`, append one row per
// record (promoting hot fields to columns, remaining Labels into a Map(String,
// String) column), and Send(). Size/flush by len(batch); honour ctx
// cancellation.
func (s *Store) Write(ctx context.Context, batch []observe.LogRecord) error {
	return fmt.Errorf("clickhouse.Write: %w", observe.ErrNotImplemented)
}

// Execute lowers the AST to SQL and streams rows or samples.
//
// TODO: lower the AST to parameterised SQL —
//   - Selectors  -> WHERE on the promoted columns / labels Map access.
//   - LineFilters -> position()/match() predicates over the line column.
//   - Aggregation -> GROUP BY toStartOfInterval(timestamp, step) [, GroupBy...]
//     with count()/quantile(q)(field) per AggOp.
//   - RawSQL set  -> run verbatim (Advanced tier) after a safety/parameter
//     guard, injecting Start/End as bound parameters.
//
// Wrap driver rows in a ResultStream; set IsMetric from Query.IsMetricQuery().
func (s *Store) Execute(ctx context.Context, q *observe.Query) (observe.ResultStream, error) {
	return nil, fmt.Errorf("clickhouse.Execute: %w", observe.ErrNotImplemented)
}

// Labels enumerates label names or values via SELECT DISTINCT.
//
// TODO: for a named Selector, SELECT <name>, count() ... GROUP BY <name> ORDER
// BY count() DESC LIMIT n (populating LabelValue.Count); for an empty Name,
// enumerate the promoted column names plus mapKeys(labels). Apply Selector.Match
// as a WHERE clause and Start/End as a time bound.
func (s *Store) Labels(ctx context.Context, sel observe.Selector) ([]observe.LabelValue, error) {
	return nil, fmt.Errorf("clickhouse.Labels: %w", observe.ErrNotImplemented)
}

// Health probes ClickHouse readiness.
//
// TODO: run `SELECT 1` against the pool and return any driver error.
func (s *Store) Health(ctx context.Context) error {
	return fmt.Errorf("clickhouse.Health: %w", observe.ErrNotImplemented)
}
