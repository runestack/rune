// Package clickhouse implements observe.LogStore against a ClickHouse backend
// (the analytical optional sink). It lowers the Query AST to SQL (see sql.go)
// and writes via batched INSERT. It supports both the Core and Advanced
// capability tiers: raw SQL mode, percentiles on arbitrary fields, and
// high-cardinality field filters.
//
// Hot fields (namespace, service, instance, node, level, stream) are promoted to
// typed columns; the remaining labels live in a Map(String, String) column and
// the raw line carries a token bloom-filter skip index. Unlike Loki, ClickHouse
// is local-disk-first, so long retention uses S3 *tiering* — a TTL move-to-volume
// against a server-configured storage policy (see schema.go).
package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
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

	// DialTimeout bounds connection establishment. Zero uses the driver default.
	DialTimeout time.Duration

	// AutoMigrate runs EnsureSchema (CREATE DATABASE/TABLE) on first connect so
	// the backend works zero-config. Operators who manage their own schema can
	// leave it false.
	AutoMigrate bool

	// --- S3 tiering (see schema.go) ---

	// StoragePolicy is the server-configured ClickHouse storage policy that
	// defines the hot (local) and cold (s3) volumes. Empty disables tiering and
	// the move-to-volume TTL.
	StoragePolicy string

	// S3Volume is the volume name within StoragePolicy that aged parts move to
	// (default "s3"). Only used when HotDays > 0.
	S3Volume string

	// HotDays moves parts older than this to the S3 volume. Zero keeps all parts
	// on the hot disk.
	HotDays int

	// RetentionDays deletes parts older than this. Zero keeps data indefinitely.
	RetentionDays int
}

// Store is the ClickHouse-backed observe.LogStore.
type Store struct {
	cfg Config

	mu         sync.Mutex
	db         *sql.DB
	schemaOnce sync.Once
	schemaErr  error
}

// New constructs a ClickHouse store. It does not dial; call Health (or any I/O
// method) to establish the connection lazily.
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
		// Parser pipeline stages are not lowered to SQL yet; RawSQL
		// (JSONExtract / extractKeyValuePairs) is the escape hatch.
		Parsers: false,
	}
}

// conn opens (once) the database handle. ClickHouse's std driver pool is lazy —
// no network round-trip happens here. When AutoMigrate is set, the schema is
// ensured on first use.
func (s *Store) conn(ctx context.Context) (*sql.DB, error) {
	s.mu.Lock()
	if s.db == nil {
		opts, err := clickhouse.ParseDSN(s.cfg.DSN)
		if err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("clickhouse: parse dsn: %w", err)
		}
		if s.cfg.DialTimeout > 0 {
			opts.DialTimeout = s.cfg.DialTimeout
		}
		s.db = clickhouse.OpenDB(opts)
	}
	db := s.db
	s.mu.Unlock()

	if s.cfg.AutoMigrate {
		s.schemaOnce.Do(func() { s.schemaErr = s.ensureSchema(ctx, db) })
		if s.schemaErr != nil {
			return nil, s.schemaErr
		}
	}
	return db, nil
}

// EnsureSchema creates the database and log table (idempotent). Safe to call at
// startup; AutoMigrate calls it lazily on first use.
func (s *Store) EnsureSchema(ctx context.Context) error {
	db, err := s.conn(ctx)
	if err != nil {
		return err
	}
	return s.ensureSchema(ctx, db)
}

func (s *Store) ensureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, buildCreateDatabaseDDL(s.cfg)); err != nil {
		return fmt.Errorf("clickhouse: create database: %w", err)
	}
	if _, err := db.ExecContext(ctx, buildCreateTableDDL(s.cfg)); err != nil {
		return fmt.Errorf("clickhouse: create table: %w", err)
	}
	return nil
}

// Write persists a batch via a single batched INSERT. The clickhouse-go std
// driver accumulates the prepared statement's Exec calls and ships them as one
// native block on Commit.
func (s *Store) Write(ctx context.Context, batch []observe.LogRecord) error {
	if len(batch) == 0 {
		return nil
	}
	db, err := s.conn(ctx)
	if err != nil {
		return err
	}
	scope, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("clickhouse: begin: %w", err)
	}
	stmt, err := scope.PrepareContext(ctx, fmt.Sprintf(
		"INSERT INTO %s (timestamp, namespace, service, instance, node, level, stream, line, labels)",
		tableRef(s.cfg.Database, s.cfg.Table),
	))
	if err != nil {
		_ = scope.Rollback()
		return fmt.Errorf("clickhouse: prepare insert: %w", err)
	}

	now := time.Now()
	for _, r := range batch {
		ts := r.Timestamp
		if ts.IsZero() {
			ts = now
		}
		labels := r.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		if _, err := stmt.ExecContext(ctx, ts, r.Namespace, r.Service, r.Instance, r.Node, r.Level, r.Stream, r.Line, labels); err != nil {
			_ = scope.Rollback()
			return fmt.Errorf("clickhouse: append row: %w", err)
		}
	}
	if err := scope.Commit(); err != nil {
		return fmt.Errorf("clickhouse: commit: %w", err)
	}
	return nil
}

// Execute lowers the AST to SQL and streams rows or samples. RawSQL is run
// verbatim (Advanced tier) with a generic column scan.
func (s *Store) Execute(ctx context.Context, q *observe.Query) (observe.ResultStream, error) {
	db, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}

	if len(q.Parsers) > 0 || len(q.LabelFilters) > 0 {
		return nil, observe.ErrCapabilityUnsupported
	}

	if q.RawSQL != "" {
		rows, err := db.QueryContext(ctx, q.RawSQL)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: raw query: %w", err)
		}
		cols, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		return &rawRowStream{rows: rows, cols: cols}, nil
	}

	if q.IsMetricQuery() {
		sqlStr, args, groupNames, err := buildMetricSQL(s.cfg.Database, s.cfg.Table, q)
		if err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, sqlStr, args...)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: metric query: %w", err)
		}
		return scanMetric(rows, groupNames)
	}

	sqlStr, args, err := buildLogSQL(s.cfg.Database, s.cfg.Table, q)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: log query: %w", err)
	}
	return &logRowStream{rows: rows}, nil
}

// Labels enumerates label names (sel.Name=="") or the ranked values of a named
// dimension via GROUP BY ... ORDER BY count() DESC.
func (s *Store) Labels(ctx context.Context, sel observe.Selector) ([]observe.LabelValue, error) {
	db, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}

	if sel.Name == "" {
		sqlStr, args, err := buildLabelNamesSQL(s.cfg.Database, s.cfg.Table, sel)
		if err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, sqlStr, args...)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: label names: %w", err)
		}
		defer rows.Close()
		// Promoted dimensions are always available; seed them first.
		out := []observe.LabelValue{
			{Name: "namespace"}, {Name: "service"}, {Name: "instance"},
			{Name: "node"}, {Name: "level"}, {Name: "stream"},
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return nil, err
			}
			out = append(out, observe.LabelValue{Name: k})
		}
		return out, rows.Err()
	}

	sqlStr, args, err := buildLabelValuesSQL(s.cfg.Database, s.cfg.Table, sel)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: label values: %w", err)
	}
	defer rows.Close()
	var out []observe.LabelValue
	for rows.Next() {
		var v string
		var c uint64
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		count := int64(math.MaxInt64)
		if c <= math.MaxInt64 {
			count = int64(c)
		}
		out = append(out, observe.LabelValue{Name: sel.Name, Value: v, Count: count})
	}
	return out, rows.Err()
}

// Health pings the ClickHouse pool.
func (s *Store) Health(ctx context.Context) error {
	db, err := s.conn(ctx)
	if err != nil {
		return err
	}
	return db.PingContext(ctx)
}

// Close releases the connection pool.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
