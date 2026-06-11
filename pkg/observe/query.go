package observe

import "time"

// Query is the parsed, backend-agnostic representation of a user query. The
// ObserveService parses a LogQL subset into this AST exactly once; the embedded
// store evaluates it in-process, the Loki store re-emits it to Loki's HTTP API,
// and the ClickHouse store lowers it to SQL. No store parses LogQL text itself.
//
// The AST is intentionally small: it expresses the Core capability tier (label
// select, line grep, count_over_time histograms, level breakdown) plus an
// escape hatch (RawSQL) for the ClickHouse-only Advanced tier.
type Query struct {
	// Selectors are the stream matchers (the `{service="api"}` part of LogQL).
	// ANDed together. Required — a query must select at least one stream.
	Selectors []Matcher

	// LineFilters are post-selection grep stages (`|= "boom"`, `!= "noise"`).
	// Applied in order; ANDed together.
	LineFilters []LineFilter

	// Parsers are field-extraction stages (`| logfmt`, `| json`) applied
	// after the line filters. Extracted fields become labels on the row —
	// visible in results, filterable via LabelFilters, and groupable in
	// aggregations. Our subset fixes the pipeline order as
	// selectors → line filters → parsers → label filters.
	Parsers []ParserStage

	// LabelFilters are post-parse predicates (`| status = "500"`,
	// `| dur > 250`) over intrinsic and extracted labels. ANDed together.
	LabelFilters []LabelFilter

	// Aggregation, when non-nil, turns the query from a log query into a
	// metric query (e.g. count_over_time → a histogram). When nil the query
	// returns raw log lines.
	Aggregation *Aggregation

	// Start and End bound the query window (event time). End is exclusive.
	Start time.Time
	End   time.Time

	// Limit caps the number of returned log lines (ignored for aggregations).
	// Zero means "store default".
	Limit int

	// Direction controls ordering of returned lines.
	Direction Direction

	// RawSQL, when non-empty, is an Advanced-tier escape hatch: the operator
	// asked for raw SQL mode. Only the ClickHouse store honours it; the Loki
	// and embedded stores must reject a Query with RawSQL set (CapabilityAdvanced
	// absent). When set, the other fields are advisory (time bounds may still be
	// injected as query parameters).
	RawSQL string
}

// IsMetricQuery reports whether the query produces aggregated samples rather
// than raw log lines.
func (q *Query) IsMetricQuery() bool { return q.Aggregation != nil }

// Direction is the sort order of returned log lines.
type Direction uint8

const (
	// DirectionBackward returns newest lines first (the default for tailing).
	DirectionBackward Direction = iota
	// DirectionForward returns oldest lines first.
	DirectionForward
)

// MatchOp is the comparison used by a stream Matcher.
type MatchOp uint8

const (
	// MatchEqual is `label="value"`.
	MatchEqual MatchOp = iota
	// MatchNotEqual is `label!="value"`.
	MatchNotEqual
	// MatchRegex is `label=~"re"`.
	MatchRegex
	// MatchNotRegex is `label!~"re"`.
	MatchNotRegex
)

// Matcher selects streams by a label dimension. Label is one of the Rune-native
// dimensions (namespace, service, instance, node) or a custom label key. The
// regex variants on a high-cardinality label key are Advanced-tier on Loki.
type Matcher struct {
	Label string
	Op    MatchOp
	Value string
}

// LineFilterOp is the operator of a line-grep stage.
type LineFilterOp uint8

const (
	// LineContains is `|= "x"` — keep lines containing x.
	LineContains LineFilterOp = iota
	// LineNotContains is `!= "x"` — drop lines containing x.
	LineNotContains
	// LineRegex is `|~ "re"` — keep lines matching re.
	LineRegex
	// LineNotRegex is `!~ "re"` — drop lines matching re.
	LineNotRegex
)

// LineFilter is a single post-selection grep stage over the log line content.
type LineFilter struct {
	Op    LineFilterOp
	Value string
}

// ParserStage is a field-extraction stage.
type ParserStage string

const (
	// ParserLogfmt extracts key=value pairs from the line (`| logfmt`).
	ParserLogfmt ParserStage = "logfmt"
	// ParserJSON extracts top-level scalar fields from a JSON line (`| json`).
	// Nested objects/arrays are skipped in this subset.
	ParserJSON ParserStage = "json"
)

// LabelFilterOp is the comparison of a post-parse label filter. String ops
// compare lexically (regex ops compile the value); numeric ops parse both
// sides as floats and drop rows whose label value is not numeric.
type LabelFilterOp string

const (
	LabelFilterEq    LabelFilterOp = "="
	LabelFilterNeq   LabelFilterOp = "!="
	LabelFilterRe    LabelFilterOp = "=~"
	LabelFilterNotRe LabelFilterOp = "!~"
	LabelFilterGT    LabelFilterOp = ">"
	LabelFilterGTE   LabelFilterOp = ">="
	LabelFilterLT    LabelFilterOp = "<"
	LabelFilterLTE   LabelFilterOp = "<="
	LabelFilterNumEq LabelFilterOp = "=="
)

// LabelFilter is one post-parse predicate over a label (intrinsic or
// extracted by a parser stage).
type LabelFilter struct {
	Label string
	Op    LabelFilterOp
	Value string
}

// AggOp is a range-vector aggregation over the matched lines.
type AggOp uint8

const (
	// AggCountOverTime is LogQL `count_over_time` — the core histogram op.
	AggCountOverTime AggOp = iota
	// AggRateOverTime is `rate` (count per second).
	AggRateOverTime
	// AggBytesOverTime is `bytes_over_time`.
	AggBytesOverTime
	// AggQuantileOverTime is a percentile over an extracted numeric field.
	// Advanced tier (ClickHouse only) — requires Quantile and Field set.
	AggQuantileOverTime
)

// Aggregation describes a metric query built over the selected log lines.
type Aggregation struct {
	// Op is the range-vector function.
	Op AggOp

	// Step is the histogram bucket width (the `[5m]` range / resolution).
	Step time.Duration

	// GroupBy is the `by (...)` grouping dimensions. Grouping by a
	// high-cardinality field is Advanced tier on Loki.
	GroupBy []string

	// Field is the extracted numeric field for AggQuantileOverTime
	// (e.g. "dur"). Ignored by the count/rate/bytes ops.
	Field string

	// Quantile in [0,1] for AggQuantileOverTime (e.g. 0.99). Advanced tier.
	Quantile float64
}
