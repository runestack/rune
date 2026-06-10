package clickhouse

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// defaultQueryLimit caps returned log lines when a Query sets Limit=0, matching
// the other backends.
const defaultQueryLimit = 1000

// promotedDims are the Rune-native identity fields stored as typed top-level
// columns (fast to filter/group). Any other label key lives in the labels Map.
var promotedDims = map[string]struct{}{
	"namespace": {}, "service": {}, "instance": {},
	"node": {}, "level": {}, "stream": {},
}

// identRe guards label keys, group dimensions, and the quantile field. Keys come
// from parsed LogQL (identifiers), so anything else is rejected rather than
// risk-injected into the SQL — values are always bound as ? parameters, but a
// map subscript key / GROUP BY expression must be inlined.
var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// dimExpr resolves a dimension name to its SQL expression: a bare column for a
// promoted dim, or a Map subscript (labels['key']) otherwise. The key is
// validated as an identifier and safe to inline.
func dimExpr(name string) (string, error) {
	if _, ok := promotedDims[name]; ok {
		return name, nil
	}
	if !identRe.MatchString(name) {
		return "", fmt.Errorf("clickhouse: invalid label key %q", name)
	}
	return "labels['" + name + "']", nil
}

func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func tableRef(db, table string) string {
	return quoteIdent(db) + "." + quoteIdent(table)
}

// buildWhere lowers selectors + line filters + the time window into a WHERE
// clause and its bound (?) arguments, in placeholder order.
func buildWhere(selectors []observe.Matcher, lineFilters []observe.LineFilter, start, end time.Time) (string, []any, error) {
	var conds []string
	var args []any

	for _, m := range selectors {
		expr, err := dimExpr(m.Label)
		if err != nil {
			return "", nil, err
		}
		switch m.Op {
		case observe.MatchEqual:
			conds = append(conds, expr+" = ?")
			args = append(args, m.Value)
		case observe.MatchNotEqual:
			conds = append(conds, expr+" != ?")
			args = append(args, m.Value)
		case observe.MatchRegex:
			conds = append(conds, "match("+expr+", ?)")
			args = append(args, m.Value)
		case observe.MatchNotRegex:
			conds = append(conds, "NOT match("+expr+", ?)")
			args = append(args, m.Value)
		default:
			return "", nil, fmt.Errorf("clickhouse: unknown match op %d", m.Op)
		}
	}

	for _, f := range lineFilters {
		switch f.Op {
		case observe.LineContains:
			conds = append(conds, "position(line, ?) > 0")
			args = append(args, f.Value)
		case observe.LineNotContains:
			conds = append(conds, "position(line, ?) = 0")
			args = append(args, f.Value)
		case observe.LineRegex:
			conds = append(conds, "match(line, ?)")
			args = append(args, f.Value)
		case observe.LineNotRegex:
			conds = append(conds, "NOT match(line, ?)")
			args = append(args, f.Value)
		default:
			return "", nil, fmt.Errorf("clickhouse: unknown line-filter op %d", f.Op)
		}
	}

	if !start.IsZero() {
		conds = append(conds, "timestamp >= ?")
		args = append(args, start)
	}
	if !end.IsZero() {
		conds = append(conds, "timestamp < ?") // End is exclusive
		args = append(args, end)
	}

	if len(conds) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

// buildLogSQL lowers a log (non-metric) query to a SELECT over the promoted
// columns + labels Map, ordered by time per Direction and capped by Limit.
func buildLogSQL(db, table string, q *observe.Query) (string, []any, error) {
	where, args, err := buildWhere(q.Selectors, q.LineFilters, q.Start, q.End)
	if err != nil {
		return "", nil, err
	}
	order := "DESC" // backward (newest first) is the default
	if q.Direction == observe.DirectionForward {
		order = "ASC"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	sql := fmt.Sprintf(
		"SELECT timestamp, line, stream, level, namespace, service, instance, node, labels FROM %s%s ORDER BY timestamp %s LIMIT %d",
		tableRef(db, table), where, order, limit,
	)
	return sql, args, nil
}

// buildMetricSQL lowers an aggregation to a bucketed GROUP BY. It returns the
// SQL, its args, and the ordered group dimension names so the caller can map
// scanned group columns back to MetricSample.GroupLabels.
func buildMetricSQL(db, table string, q *observe.Query) (sql string, args []any, groupNames []string, err error) {
	agg := q.Aggregation
	stepSec := int64(agg.Step / time.Second)
	if stepSec <= 0 {
		stepSec = 60
	}

	where, args, err := buildWhere(q.Selectors, q.LineFilters, q.Start, q.End)
	if err != nil {
		return "", nil, nil, err
	}

	bucket := fmt.Sprintf("toStartOfInterval(timestamp, INTERVAL %d SECOND)", stepSec)
	selectCols := []string{bucket + " AS bucket"}
	groupExprs := []string{"bucket"}
	for _, g := range agg.GroupBy {
		expr, derr := dimExpr(g)
		if derr != nil {
			return "", nil, nil, derr
		}
		selectCols = append(selectCols, expr+" AS "+quoteIdent(g))
		groupExprs = append(groupExprs, expr)
		groupNames = append(groupNames, g)
	}

	var aggExpr string
	switch agg.Op {
	case observe.AggCountOverTime:
		aggExpr = "toFloat64(count())"
	case observe.AggRateOverTime:
		aggExpr = fmt.Sprintf("count() / %d", stepSec) // events per second
	case observe.AggBytesOverTime:
		aggExpr = "toFloat64(sum(length(line)))"
	case observe.AggQuantileOverTime:
		fexpr, derr := dimExpr(agg.Field)
		if derr != nil {
			return "", nil, nil, derr
		}
		aggExpr = fmt.Sprintf("ifNull(quantile(%s)(toFloat64OrNull(%s)), 0)", formatFloat(agg.Quantile), fexpr)
	default:
		return "", nil, nil, fmt.Errorf("clickhouse: unknown aggregation op %d", agg.Op)
	}
	selectCols = append(selectCols, aggExpr+" AS value")

	sql = fmt.Sprintf(
		"SELECT %s FROM %s%s GROUP BY %s ORDER BY bucket ASC",
		strings.Join(selectCols, ", "), tableRef(db, table), where, strings.Join(groupExprs, ", "),
	)
	return sql, args, groupNames, nil
}

// buildLabelValuesSQL counts the distinct values of a named dimension, ranked by
// frequency (populating LabelValue.Count).
func buildLabelValuesSQL(db, table string, sel observe.Selector) (string, []any, error) {
	expr, err := dimExpr(sel.Name)
	if err != nil {
		return "", nil, err
	}
	where, args, err := buildWhere(sel.Match, nil, sel.Start, sel.End)
	if err != nil {
		return "", nil, err
	}
	// Exclude empty values (a missing label reads as '').
	notEmpty := expr + " != ''"
	if where == "" {
		where = " WHERE " + notEmpty
	} else {
		where += " AND " + notEmpty
	}
	limit := sel.Limit
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	sql := fmt.Sprintf(
		"SELECT %s AS v, count() AS c FROM %s%s GROUP BY v ORDER BY c DESC, v ASC LIMIT %d",
		expr, tableRef(db, table), where, limit,
	)
	return sql, args, nil
}

// buildLabelNamesSQL enumerates the custom label keys present in the window. The
// promoted dimension names are added by the caller (they are always present).
func buildLabelNamesSQL(db, table string, sel observe.Selector) (string, []any, error) {
	where, args, err := buildWhere(sel.Match, nil, sel.Start, sel.End)
	if err != nil {
		return "", nil, err
	}
	limit := sel.Limit
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	sql := fmt.Sprintf(
		"SELECT DISTINCT arrayJoin(mapKeys(labels)) AS k FROM %s%s ORDER BY k ASC LIMIT %d",
		tableRef(db, table), where, limit,
	)
	return sql, args, nil
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
