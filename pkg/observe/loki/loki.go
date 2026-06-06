// Package loki implements observe.LogStore against a Loki backend (the light
// optional sink). It is Core-tier only: it re-emits the Query AST to Loki's
// HTTP API and writes via Loki's push API. It never parses LogQL — the
// ObserveService does that and hands this store the parsed AST.
//
// Skeleton: the struct, constructor, and Capabilities are real; the I/O bodies
// return observe.ErrNotImplemented pending the adapter port (plan §6.4, §7
// step 6). See _docs/plugins/RUNESIGHT_IMPLEMENTATION_PLAN.md.
package loki

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// Compile-time assertion that Store satisfies the seam.
var _ observe.LogStore = (*Store)(nil)

// Config configures the Loki store.
type Config struct {
	// BaseURL is the Loki HTTP endpoint (e.g. http://loki:3100).
	BaseURL string

	// TenantID is sent as the X-Scope-OrgID header for multi-tenant Loki.
	// Empty for single-tenant deployments.
	TenantID string

	// Timeout bounds a single HTTP request to Loki. Zero uses a default.
	Timeout time.Duration

	// HTTPClient overrides the client used for requests. nil uses a default.
	HTTPClient *http.Client
}

// Store is the Loki-backed observe.LogStore.
type Store struct {
	cfg  Config
	http *http.Client
}

// New constructs a Loki store. It does not dial Loki; call Health to verify
// connectivity.
func New(cfg Config) (*Store, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("loki: BaseURL is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &Store{cfg: cfg, http: hc}, nil
}

// Capabilities reports the Loki capability handshake: Core tier only. No raw
// SQL, no percentiles, no cheap high-cardinality filters (cardinality is Loki's
// OOM failure mode).
func (s *Store) Capabilities() observe.Capabilities {
	return observe.Capabilities{
		Backend:                "loki",
		MaxTier:                observe.TierCore,
		RawSQL:                 false,
		Percentiles:            false,
		HighCardinalityFilters: false,
	}
}

// Write persists a batch via Loki's push API.
//
// TODO: group records into Loki streams keyed by LogRecord.StreamLabels(),
// build the POST /loki/api/v1/push payload (streams[].values = [[ts_ns, line]]),
// gzip it, set Content-Type: application/json and the tenant header, and POST
// to BaseURL. Surface 4xx as permanent errors and 5xx as retryable.
func (s *Store) Write(ctx context.Context, batch []observe.LogRecord) error {
	return fmt.Errorf("loki.Write: %w", observe.ErrNotImplemented)
}

// Execute runs a Core-tier query against Loki.
//
// TODO: render the AST back into a LogQL string (the inverse of the
// ObserveService parser): Selectors -> `{k="v"}`, LineFilters -> `|= "x"`, an
// Aggregation -> `count_over_time({...}[step])`. Choose the query endpoint by
// Query.IsMetricQuery: GET /loki/api/v1/query_range for metrics, the same for
// log lines with direction/limit. Wrap the streamed JSON in a ResultStream.
// Reject any Query whose RawSQL is set or that needs Advanced tier with
// observe.ErrCapabilityUnsupported.
func (s *Store) Execute(ctx context.Context, q *observe.Query) (observe.ResultStream, error) {
	if q.RawSQL != "" {
		return nil, observe.ErrCapabilityUnsupported
	}
	return nil, fmt.Errorf("loki.Execute: %w", observe.ErrNotImplemented)
}

// Labels enumerates label names or values via Loki's label API.
//
// TODO: map Selector.Name=="" to GET /loki/api/v1/labels and a named selector
// to GET /loki/api/v1/label/<name>/values, passing start/end and any match[]
// constraints derived from Selector.Match. Loki does not return counts, so
// LabelValue.Count stays 0.
func (s *Store) Labels(ctx context.Context, sel observe.Selector) ([]observe.LabelValue, error) {
	return nil, fmt.Errorf("loki.Labels: %w", observe.ErrNotImplemented)
}

// Health probes Loki readiness.
//
// TODO: GET /ready against BaseURL and return a non-nil error on any status
// other than 200.
func (s *Store) Health(ctx context.Context) error {
	return fmt.Errorf("loki.Health: %w", observe.ErrNotImplemented)
}
