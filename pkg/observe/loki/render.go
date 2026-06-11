package loki

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// renderLogQL re-emits a Query AST as a LogQL string — the inverse of
// observe.ParseLogQL. The ObserveService parses LogQL into the AST once; the
// Loki store renders it back out because Loki's HTTP API speaks LogQL. The
// embedded store evaluates the AST directly and the ClickHouse store lowers it
// to SQL, so this renderer is Loki-specific.
//
// Returns observe.ErrCapabilityUnsupported for anything beyond the Core tier
// (e.g. quantile_over_time), mirroring the Loki capability handshake.
func renderLogQL(q *observe.Query) (string, error) {
	sel := renderSelectors(q.Selectors)
	filters := renderLineFilters(q.LineFilters) + renderPipeline(q.Parsers, q.LabelFilters)

	if q.Aggregation == nil {
		return sel + filters, nil
	}

	agg := q.Aggregation
	var fn string
	switch agg.Op {
	case observe.AggCountOverTime:
		fn = "count_over_time"
	case observe.AggRateOverTime:
		fn = "rate"
	case observe.AggBytesOverTime:
		fn = "bytes_over_time"
	default:
		// AggQuantileOverTime is Advanced tier (ClickHouse only).
		return "", observe.ErrCapabilityUnsupported
	}

	// Line filters live inside the range vector: count_over_time({sel} |= "x" [5m]).
	inner := fmt.Sprintf("%s(%s%s [%s])", fn, sel, filters, formatLokiDuration(agg.Step))
	if len(agg.GroupBy) > 0 {
		inner = fmt.Sprintf("sum by (%s) (%s)", strings.Join(agg.GroupBy, ", "), inner)
	}
	return inner, nil
}

// renderSelectors renders the `{label="v", label2=~"re"}` stream selector.
func renderSelectors(ms []observe.Matcher) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.Label + matchOpString(m.Op) + strconv.Quote(m.Value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// renderLineFilters renders a ` |= "x" != "y" |~ "re" !~ "re"` grep chain. Each
// stage is prefixed with a space so it appends cleanly to the selector.
func renderLineFilters(fs []observe.LineFilter) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteByte(' ')
		b.WriteString(lineOpString(f.Op))
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(f.Value))
	}
	return b.String()
}

// renderPipeline renders parser stages and post-parse label filters in our
// subset's fixed order (after the line filters) — natively valid Loki LogQL.
func renderPipeline(parsers []observe.ParserStage, lfs []observe.LabelFilter) string {
	var b strings.Builder
	for _, p := range parsers {
		b.WriteString(" | ")
		b.WriteString(string(p))
	}
	for _, f := range lfs {
		b.WriteString(" | ")
		b.WriteString(f.Label)
		b.WriteByte(' ')
		b.WriteString(string(f.Op))
		b.WriteByte(' ')
		switch f.Op {
		case observe.LabelFilterEq, observe.LabelFilterNeq, observe.LabelFilterRe, observe.LabelFilterNotRe:
			b.WriteString(strconv.Quote(f.Value))
		default: // numeric ops take bare values
			b.WriteString(f.Value)
		}
	}
	return b.String()
}

func matchOpString(op observe.MatchOp) string {
	switch op {
	case observe.MatchEqual:
		return "="
	case observe.MatchNotEqual:
		return "!="
	case observe.MatchRegex:
		return "=~"
	case observe.MatchNotRegex:
		return "!~"
	default:
		return "="
	}
}

func lineOpString(op observe.LineFilterOp) string {
	switch op {
	case observe.LineContains:
		return "|="
	case observe.LineNotContains:
		return "!="
	case observe.LineRegex:
		return "|~"
	case observe.LineNotRegex:
		return "!~"
	default:
		return "|="
	}
}

// formatLokiDuration renders a Go duration in the Prometheus/Loki range syntax.
// It uses integer seconds when the duration is a whole number of seconds and
// integer milliseconds otherwise, so the output never has a fractional unit
// (which Loki's range parser rejects). Non-positive steps default to 1m.
func formatLokiDuration(d time.Duration) string {
	if d <= 0 {
		return "1m"
	}
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
}
