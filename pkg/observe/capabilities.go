package observe

// Tier is a capability tier a store reports. The dashboard feature-flags
// advanced widgets (SQL mode, percentiles, cross-stream joins) off the reported
// tier so the embedded store, Loki and ClickHouse are not forced to capability
// parity (plan §4).
type Tier uint8

const (
	// TierCore is supported by every install: label select, line grep,
	// count_over_time histograms, and level breakdown.
	TierCore Tier = iota

	// TierAdvanced is ClickHouse-only: raw SQL mode, percentiles on arbitrary
	// fields, cross-stream joins, and high-cardinality field filters.
	TierAdvanced
)

// String renders the tier for logs and the capability handshake.
func (t Tier) String() string {
	switch t {
	case TierCore:
		return "core"
	case TierAdvanced:
		return "advanced"
	default:
		return "unknown"
	}
}

// Capabilities is the handshake a store reports on connect. The ObserveService
// forwards it to the dashboard; the dashboard shows SQL mode and advanced
// widgets only when the backend reports TierAdvanced.
type Capabilities struct {
	// Backend is the store identity ("embedded", "loki", or "clickhouse"),
	// surfaced in the dashboard and in operational logs.
	Backend string

	// MaxTier is the highest tier the store supports. The embedded store and
	// Loki report TierCore; ClickHouse reports TierAdvanced (which implies
	// Core).
	MaxTier Tier

	// RawSQL is true when the store accepts Query.RawSQL (ClickHouse only).
	RawSQL bool

	// Percentiles is true when AggQuantileOverTime is supported.
	Percentiles bool

	// HighCardinalityFilters is true when filtering/grouping on arbitrary
	// high-cardinality label keys (e.g. order_id, trace_id) is cheap enough to
	// expose in the dashboard.
	HighCardinalityFilters bool

	// Parsers is true when the store evaluates field-extraction pipeline
	// stages (`| logfmt`, `| json`) and post-parse label filters. The
	// embedded store evaluates them in-process and Loki natively; the
	// ClickHouse adapter rejects them for now (RawSQL is its escape hatch).
	Parsers bool
}

// Supports reports whether the store can serve the given tier.
func (c Capabilities) Supports(t Tier) bool { return t <= c.MaxTier }
