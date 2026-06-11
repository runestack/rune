package observe

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseLogQL parses the Core-tier LogQL subset into a Query AST. The
// ObserveService calls this exactly once per request; stores never see raw
// LogQL text (plan §4). start/end/limit/forward come from the request envelope,
// not the LogQL string.
//
// Supported subset (Core tier):
//
//	{label="v", label2=~"re"} |= "needle" != "noise" |~ "re" !~ "re"
//	count_over_time({label="v"}[5m])
//	rate({label="v"}[5m])
//	bytes_over_time({label="v"}[5m])
//	sum by (level) (count_over_time({label="v"}[5m]))
//
// Anything outside this subset (parsers, unwrap, label_format, joins,
// quantile_over_time, ...) is Advanced tier and must go through RawSQL on a
// ClickHouse backend; ParseLogQL returns an error for it.
func ParseLogQL(logql string, start, end time.Time, limit int, forward bool) (*Query, error) {
	expr := strings.TrimSpace(logql)
	if expr == "" {
		return nil, fmt.Errorf("observe: empty LogQL query")
	}

	q := &Query{Start: start, End: end, Limit: limit}
	if forward {
		q.Direction = DirectionForward
	} else {
		q.Direction = DirectionBackward
	}

	// Metric query forms: optional `sum by (...) (` wrapper around a range
	// aggregation.
	if agg, inner, groupBy, ok, err := parseMetricWrapper(expr); err != nil {
		return nil, err
	} else if ok {
		sel, pipe, rerr := parseStreamAndFilters(inner)
		if rerr != nil {
			return nil, rerr
		}
		q.Selectors = sel
		q.LineFilters = pipe.lineFilters
		q.Parsers = pipe.parsers
		q.LabelFilters = pipe.labelFilters
		agg.GroupBy = groupBy
		q.Aggregation = agg
		return q, nil
	}

	// Plain log query: a stream selector plus an optional pipeline.
	sel, pipe, err := parseStreamAndFilters(expr)
	if err != nil {
		return nil, err
	}
	q.Selectors = sel
	q.LineFilters = pipe.lineFilters
	q.Parsers = pipe.parsers
	q.LabelFilters = pipe.labelFilters
	return q, nil
}

// parseMetricWrapper detects and unwraps `sum by (...) ( <rangeAgg> )` and bare
// range aggregations like `count_over_time({...}[5m])`. Returns ok=false when
// expr is a plain log query.
func parseMetricWrapper(expr string) (agg *Aggregation, inner string, groupBy []string, ok bool, err error) {
	rest := expr
	if strings.HasPrefix(rest, "sum") {
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "sum"))
		if strings.HasPrefix(rest, "by") {
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "by"))
			gb, after, gerr := parseByClause(rest)
			if gerr != nil {
				return nil, "", nil, false, gerr
			}
			groupBy = gb
			rest = strings.TrimSpace(after)
		}
		// Expect a parenthesised aggregation expression.
		body, perr := unwrapParens(rest)
		if perr != nil {
			return nil, "", nil, false, perr
		}
		rest = strings.TrimSpace(body)
	}

	for _, m := range []struct {
		kw string
		op AggOp
	}{
		{"count_over_time", AggCountOverTime},
		{"rate", AggRateOverTime},
		{"bytes_over_time", AggBytesOverTime},
	} {
		if !strings.HasPrefix(rest, m.kw+"(") && !strings.HasPrefix(rest, m.kw+" (") {
			continue
		}
		body, perr := unwrapParens(strings.TrimPrefix(rest, m.kw))
		if perr != nil {
			return nil, "", nil, false, perr
		}
		streamPart, step, serr := splitRange(body)
		if serr != nil {
			return nil, "", nil, false, serr
		}
		return &Aggregation{Op: m.op, Step: step}, streamPart, groupBy, true, nil
	}

	if expr != rest {
		// We consumed a sum/by wrapper but found no recognised aggregation.
		return nil, "", nil, false, fmt.Errorf("observe: unsupported metric expression %q", expr)
	}
	return nil, "", nil, false, nil
}

// parseByClause parses `(a, b, c) <rest>` returning the group labels and the
// remainder.
func parseByClause(s string) (labels []string, rest string, err error) {
	if !strings.HasPrefix(s, "(") {
		return nil, "", fmt.Errorf("observe: expected '(' after 'by'")
	}
	close := strings.Index(s, ")")
	if close < 0 {
		return nil, "", fmt.Errorf("observe: unterminated 'by (...)' clause")
	}
	inner := s[1:close]
	for _, p := range strings.Split(inner, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			labels = append(labels, p)
		}
	}
	return labels, s[close+1:], nil
}

// unwrapParens expects s to start with '(' and returns the balanced inner
// content. Trailing characters after the matching ')' are an error.
func unwrapParens(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "(") {
		return "", fmt.Errorf("observe: expected '('")
	}
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				if strings.TrimSpace(s[i+1:]) != "" {
					return "", fmt.Errorf("observe: trailing characters after ')': %q", s[i+1:])
				}
				return s[1:i], nil
			}
		}
	}
	return "", fmt.Errorf("observe: unbalanced parentheses in %q", s)
}

// splitRange splits a `{...}[5m]` body into the stream selector text and the
// range duration.
func splitRange(s string) (streamPart string, step time.Duration, err error) {
	open := strings.LastIndex(s, "[")
	closeB := strings.LastIndex(s, "]")
	if open < 0 || closeB < 0 || closeB < open {
		return "", 0, fmt.Errorf("observe: missing [range] in %q", s)
	}
	rangeStr := strings.TrimSpace(s[open+1 : closeB])
	d, derr := time.ParseDuration(rangeStr)
	if derr != nil {
		return "", 0, fmt.Errorf("observe: invalid range %q: %w", rangeStr, derr)
	}
	return strings.TrimSpace(s[:open]), d, nil
}

// pipeline is the parsed post-selector chain: grep stages, parser stages, and
// post-parse label filters. Our subset fixes evaluation order as
// line filters → parsers → label filters regardless of written order.
type pipeline struct {
	lineFilters  []LineFilter
	parsers      []ParserStage
	labelFilters []LabelFilter
}

// parseStreamAndFilters parses `{matchers} |= "x" | logfmt | dur > 250` into
// selectors and the pipeline.
func parseStreamAndFilters(s string) ([]Matcher, pipeline, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil, pipeline{}, fmt.Errorf("observe: query must start with a stream selector '{...}'")
	}
	close := strings.Index(s, "}")
	if close < 0 {
		return nil, pipeline{}, fmt.Errorf("observe: unterminated stream selector")
	}
	matchers, err := parseMatchers(s[1:close])
	if err != nil {
		return nil, pipeline{}, err
	}
	if len(matchers) == 0 {
		return nil, pipeline{}, fmt.Errorf("observe: stream selector must have at least one matcher")
	}
	pipe, err := parsePipeline(s[close+1:])
	if err != nil {
		return nil, pipeline{}, err
	}
	return matchers, pipe, nil
}

// parseMatchers parses `a="b", c=~"re"` into Matchers.
func parseMatchers(s string) ([]Matcher, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []Matcher
	for _, part := range splitTopLevel(s, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m, err := parseMatcher(part)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func parseMatcher(s string) (Matcher, error) {
	// Order matters: check two-char operators before '='.
	for _, opdef := range []struct {
		tok string
		op  MatchOp
	}{
		{"!~", MatchNotRegex},
		{"=~", MatchRegex},
		{"!=", MatchNotEqual},
		{"=", MatchEqual},
	} {
		if idx := strings.Index(s, opdef.tok); idx > 0 {
			label := strings.TrimSpace(s[:idx])
			val, err := unquote(strings.TrimSpace(s[idx+len(opdef.tok):]))
			if err != nil {
				return Matcher{}, err
			}
			return Matcher{Label: label, Op: opdef.op, Value: val}, nil
		}
	}
	return Matcher{}, fmt.Errorf("observe: invalid matcher %q", s)
}

// parsePipeline parses the trailing stage chain: line filters (`|= "x"`,
// `!= "y"`, `|~`/`!~`), parser stages (`| logfmt`, `| json`), and post-parse
// label filters (`| status = "500"`, `| dur > 250`).
func parsePipeline(s string) (pipeline, error) {
	var pipe pipeline
	s = strings.TrimSpace(s)
	for s != "" {
		switch {
		case strings.HasPrefix(s, "|="):
			val, rest, err := takeQuoted(strings.TrimSpace(s[2:]))
			if err != nil {
				return pipe, err
			}
			pipe.lineFilters = append(pipe.lineFilters, LineFilter{Op: LineContains, Value: val})
			s = strings.TrimSpace(rest)
		case strings.HasPrefix(s, "|~"):
			val, rest, err := takeQuoted(strings.TrimSpace(s[2:]))
			if err != nil {
				return pipe, err
			}
			pipe.lineFilters = append(pipe.lineFilters, LineFilter{Op: LineRegex, Value: val})
			s = strings.TrimSpace(rest)
		case strings.HasPrefix(s, "!~"):
			val, rest, err := takeQuoted(strings.TrimSpace(s[2:]))
			if err != nil {
				return pipe, err
			}
			pipe.lineFilters = append(pipe.lineFilters, LineFilter{Op: LineNotRegex, Value: val})
			s = strings.TrimSpace(rest)
		case strings.HasPrefix(s, "!="):
			val, rest, err := takeQuoted(strings.TrimSpace(s[2:]))
			if err != nil {
				return pipe, err
			}
			pipe.lineFilters = append(pipe.lineFilters, LineFilter{Op: LineNotContains, Value: val})
			s = strings.TrimSpace(rest)
		case strings.HasPrefix(s, "|"):
			rest, err := parsePipeStage(strings.TrimSpace(s[1:]), &pipe)
			if err != nil {
				return pipe, err
			}
			s = strings.TrimSpace(rest)
		default:
			return pipe, fmt.Errorf("observe: unexpected token in pipeline: %q", s)
		}
	}
	return pipe, nil
}

// parsePipeStage parses what follows a bare `|`: a parser stage name or a
// label-filter expression `ident op value`. Returns the unconsumed remainder.
func parsePipeStage(s string, pipe *pipeline) (string, error) {
	ident, rest := takeIdent(s)
	if ident == "" {
		return "", fmt.Errorf("observe: expected parser stage or label filter after '|', got %q", s)
	}
	rest = strings.TrimSpace(rest)

	// Label filter when an operator follows (so a label literally named
	// "json" stays filterable); parser stage otherwise.
	op, afterOp, hasOp := takeLabelFilterOp(rest)
	if !hasOp {
		switch ParserStage(ident) {
		case ParserLogfmt, ParserJSON:
			pipe.parsers = append(pipe.parsers, ParserStage(ident))
			return rest, nil
		default:
			return "", fmt.Errorf("observe: unknown pipeline stage %q (want logfmt, json, or a label filter)", ident)
		}
	}

	afterOp = strings.TrimSpace(afterOp)
	var val, remainder string
	if strings.HasPrefix(afterOp, "\"") {
		v, r, err := takeQuoted(afterOp)
		if err != nil {
			return "", err
		}
		val, remainder = v, r
	} else {
		val, remainder = takeBareToken(afterOp)
		if val == "" {
			return "", fmt.Errorf("observe: label filter %s %s missing value", ident, op)
		}
		if op == LabelFilterRe || op == LabelFilterNotRe {
			return "", fmt.Errorf("observe: regex label filter value must be quoted: %s %s %s", ident, op, val)
		}
	}
	pipe.labelFilters = append(pipe.labelFilters, LabelFilter{Label: ident, Op: op, Value: val})
	return remainder, nil
}

// takeIdent reads a leading identifier ([A-Za-z_][A-Za-z0-9_]*).
func takeIdent(s string) (ident, rest string) {
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		isAlpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i == 0 && !isAlpha {
			return "", s
		}
		if !isAlpha && !(c >= '0' && c <= '9') {
			break
		}
	}
	return s[:i], s[i:]
}

// takeLabelFilterOp reads a leading label-filter operator, longest first.
func takeLabelFilterOp(s string) (LabelFilterOp, string, bool) {
	for _, op := range []LabelFilterOp{
		LabelFilterRe, LabelFilterNotRe, LabelFilterGTE, LabelFilterLTE,
		LabelFilterNumEq, LabelFilterNeq, LabelFilterEq, LabelFilterGT, LabelFilterLT,
	} {
		if strings.HasPrefix(s, string(op)) {
			return op, s[len(op):], true
		}
	}
	return "", s, false
}

// takeBareToken reads up to the next whitespace or '|'.
func takeBareToken(s string) (tok, rest string) {
	i := 0
	for ; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '|' {
			break
		}
	}
	return s[:i], s[i:]
}

// takeQuoted reads a leading "..." string and returns its unquoted value plus
// the remainder.
func takeQuoted(s string) (val, rest string, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "\"") {
		return "", "", fmt.Errorf("observe: expected quoted string, got %q", s)
	}
	// Find the closing quote, honouring backslash escapes.
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			v, uerr := strconv.Unquote(s[:i+1])
			if uerr != nil {
				return "", "", fmt.Errorf("observe: bad quoted string: %w", uerr)
			}
			return v, s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("observe: unterminated quoted string in %q", s)
}

// unquote unquotes a "..." literal.
func unquote(s string) (string, error) {
	if len(s) >= 2 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
		v, err := strconv.Unquote(s)
		if err != nil {
			return "", fmt.Errorf("observe: bad quoted value %q: %w", s, err)
		}
		return v, nil
	}
	return "", fmt.Errorf("observe: matcher value must be quoted, got %q", s)
}

// splitTopLevel splits s on sep, ignoring sep inside double quotes.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(s):
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == sep && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	return parts
}
