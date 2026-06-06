// Package backend selects and constructs the observe.LogStore for the
// runefile-configured backend value (plan §5). It lives in its own package so
// the core observe package stays free of store imports (avoiding an import
// cycle): the stores depend on observe, and backend depends on both.
//
// The control plane calls Open after parsing the [observability] runefile block
// so the rest of the program is backend-agnostic.
package backend

import (
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/observe/clickhouse"
	"github.com/runestack/rune/pkg/observe/embedded"
	"github.com/runestack/rune/pkg/observe/loki"
)

// Backend names the runefile storage choice. The [observability].backend value
// selects the store.
type Backend string

const (
	// BackendEmbedded is the in-process default (Core tier). Ships in runed.
	BackendEmbedded Backend = "embedded"
	// BackendLoki is the light optional sink (Core tier only).
	BackendLoki Backend = "loki"
	// BackendClickHouse is the analytical optional sink (Core + Advanced tiers).
	BackendClickHouse Backend = "clickhouse"
)

// EmbeddedConfig mirrors embedded.Config without forcing callers to import the
// embedded package directly.
type EmbeddedConfig struct {
	RetentionDays int
	MaxRecords    int
}

// LokiConfig mirrors loki.Config so callers can configure the Loki backend
// without importing the store package directly.
type LokiConfig struct {
	BaseURL  string
	TenantID string
}

// ClickHouseConfig mirrors clickhouse.Config for the same reason.
type ClickHouseConfig struct {
	DSN      string
	Database string
	Table    string
}

// Options bundle every backend's config; Open uses the one matching the
// selected backend.
type Options struct {
	Embedded   EmbeddedConfig
	Loki       LokiConfig
	ClickHouse ClickHouseConfig
	Logger     log.Logger
}

// Open constructs the LogStore for the given backend. An empty backend defaults
// to embedded so a bare `[observability] enabled = true` block works out of the
// box (plan §5).
func Open(b Backend, opts Options) (observe.LogStore, error) {
	switch b {
	case BackendEmbedded, "":
		retention := embedded.DefaultRetention
		if opts.Embedded.RetentionDays > 0 {
			retention = time.Duration(opts.Embedded.RetentionDays) * 24 * time.Hour
		}
		return embedded.New(embedded.Config{
			Retention:  retention,
			MaxRecords: opts.Embedded.MaxRecords,
			Logger:     opts.Logger,
		}), nil
	case BackendLoki:
		return loki.New(loki.Config{BaseURL: opts.Loki.BaseURL, TenantID: opts.Loki.TenantID})
	case BackendClickHouse:
		return clickhouse.New(clickhouse.Config{
			DSN:      opts.ClickHouse.DSN,
			Database: opts.ClickHouse.Database,
			Table:    opts.ClickHouse.Table,
		})
	default:
		return nil, fmt.Errorf("unknown observability backend %q (want embedded|loki|clickhouse)", b)
	}
}
