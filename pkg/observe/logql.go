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
		sel, filters, rerr := parseStreamAndFilters(inner)
		if rerr != nil {
			return nil, rerr
		}
		q.Selectors = sel
		q.LineFilters = filters
		agg.GroupBy = groupBy
		q.Aggregation = agg
		return q, nil
	}

	// Plain log query: a stream selector plus optional line filters.
	sel, filters, err := parseStreamAndFilters(expr)
	if err != nil {
		return nil, err
	}
	q.Selectors = sel
	q.LineFilters = filters
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

// parseStreamAndFilters parses `{matchers} |= "x" != "y"` into selectors and
// line filters.
func parseStreamAndFilters(s string) ([]Matcher, []LineFilter, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil, nil, fmt.Errorf("observe: query must start with a stream selector '{...}'")
	}
	close := strings.Index(s, "}")
	if close < 0 {
		return nil, nil, fmt.Errorf("observe: unterminated stream selector")
	}
	matchers, err := parseMatchers(s[1:close])
	if err != nil {
		return nil, nil, err
	}
	if len(matchers) == 0 {
		return nil, nil, fmt.Errorf("observe: stream selector must have at least one matcher")
	}
	filters, err := parseLineFilters(s[close+1:])
	if err != nil {
		return nil, nil, err
	}
	return matchers, filters, nil
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

// parseLineFilters parses a trailing `|= "x" != "y" |~ "re" !~ "re"` chain.
func parseLineFilters(s string) ([]LineFilter, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []LineFilter
	for s != "" {
		var op LineFilterOp
		var tok string
		switch {
		case strings.HasPrefix(s, "|="):
			op, tok = LineContains, "|="
		case strings.HasPrefix(s, "!~"):
			op, tok = LineNotRegex, "!~"
		case strings.HasPrefix(s, "!="):
			op, tok = LineNotContains, "!="
		case strings.HasPrefix(s, "|~"):
			op, tok = LineRegex, "|~"
		default:
			return nil, fmt.Errorf("observe: unexpected token in line filters: %q", s)
		}
		s = strings.TrimSpace(strings.TrimPrefix(s, tok))
		val, rest, err := takeQuoted(s)
		if err != nil {
			return nil, err
		}
		out = append(out, LineFilter{Op: op, Value: val})
		s = strings.TrimSpace(rest)
	}
	return out, nil
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
