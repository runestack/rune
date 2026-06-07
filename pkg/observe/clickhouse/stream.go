package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// logRowStream lazily streams the structured log-query result, scanning one row
// per Next so a large (LIMIT-bounded) result never fully materialises.
type logRowStream struct {
	rows *sql.Rows
	cur  observe.LogRow
	err  error
	done bool
}

func (s *logRowStream) IsMetric() bool { return false }

func (s *logRowStream) Next(ctx context.Context) bool {
	if s.done || s.err != nil {
		return false
	}
	if err := ctx.Err(); err != nil {
		s.err = err
		return false
	}
	if !s.rows.Next() {
		s.done = true
		s.err = s.rows.Err()
		return false
	}
	var (
		ts                                 time.Time
		line, stream, level                string
		namespace, service, instance, node string
		labels                             map[string]string
	)
	if err := s.rows.Scan(&ts, &line, &stream, &level, &namespace, &service, &instance, &node, &labels); err != nil {
		s.err = err
		return false
	}
	if labels == nil {
		labels = map[string]string{}
	}
	// Surface the promoted identity dims alongside custom labels (mirrors
	// LogRecord.StreamLabels so all backends present the same row shape).
	labels["namespace"] = namespace
	labels["service"] = service
	labels["instance"] = instance
	labels["node"] = node

	s.cur = observe.LogRow{
		Timestamp: ts.UTC(),
		Line:      line,
		Stream:    stream,
		Level:     level,
		Labels:    labels,
	}
	return true
}

func (s *logRowStream) Row() observe.LogRow          { return s.cur }
func (s *logRowStream) Sample() observe.MetricSample { return observe.MetricSample{} }
func (s *logRowStream) Err() error                   { return s.err }
func (s *logRowStream) Close() error                 { return s.rows.Close() }

// rawRowStream streams an Advanced-tier RawSQL result. The column set is
// arbitrary, so each row becomes a LogRow whose Labels carry every column by
// name; well-known columns (timestamp/line/level/stream) are also lifted into
// the typed fields for the dashboard.
type rawRowStream struct {
	rows *sql.Rows
	cols []string
	cur  observe.LogRow
	err  error
	done bool
}

func (s *rawRowStream) IsMetric() bool { return false }

func (s *rawRowStream) Next(ctx context.Context) bool {
	if s.done || s.err != nil {
		return false
	}
	if err := ctx.Err(); err != nil {
		s.err = err
		return false
	}
	if !s.rows.Next() {
		s.done = true
		s.err = s.rows.Err()
		return false
	}
	vals := make([]any, len(s.cols))
	ptrs := make([]any, len(s.cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := s.rows.Scan(ptrs...); err != nil {
		s.err = err
		return false
	}
	row := observe.LogRow{Labels: make(map[string]string, len(s.cols))}
	for i, c := range s.cols {
		v := vals[i]
		sv := stringifyValue(v)
		switch c {
		case "timestamp":
			if t, ok := v.(time.Time); ok {
				row.Timestamp = t.UTC()
			}
		case "line":
			row.Line = sv
		case "level":
			row.Level = sv
		case "stream":
			row.Stream = sv
		}
		row.Labels[c] = sv
	}
	s.cur = row
	return true
}

func (s *rawRowStream) Row() observe.LogRow          { return s.cur }
func (s *rawRowStream) Sample() observe.MetricSample { return observe.MetricSample{} }
func (s *rawRowStream) Err() error                   { return s.err }
func (s *rawRowStream) Close() error                 { return s.rows.Close() }

// metricStream replays a fully-buffered aggregation result. Aggregations are
// bucket-bounded (small), so materialising is cheap and keeps Close/Err simple.
type metricStream struct {
	samples []observe.MetricSample
	idx     int
}

func (s *metricStream) IsMetric() bool { return true }

func (s *metricStream) Next(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if s.idx+1 >= len(s.samples) {
		s.idx = len(s.samples)
		return false
	}
	s.idx++
	return true
}

func (s *metricStream) Row() observe.LogRow { return observe.LogRow{} }
func (s *metricStream) Sample() observe.MetricSample {
	if s.idx >= 0 && s.idx < len(s.samples) {
		return s.samples[s.idx]
	}
	return observe.MetricSample{}
}
func (s *metricStream) Err() error   { return nil }
func (s *metricStream) Close() error { return nil }

// scanMetric materialises a bucketed aggregation: bucket time, the ordered group
// columns, then the float value.
func scanMetric(rows *sql.Rows, groupNames []string) (observe.ResultStream, error) {
	defer rows.Close()
	var samples []observe.MetricSample
	for rows.Next() {
		var bucket time.Time
		groupVals := make([]string, len(groupNames))
		var value float64

		dest := make([]any, 0, 2+len(groupNames))
		dest = append(dest, &bucket)
		for i := range groupVals {
			dest = append(dest, &groupVals[i])
		}
		dest = append(dest, &value)

		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		var gl map[string]string
		if len(groupNames) > 0 {
			gl = make(map[string]string, len(groupNames))
			for i, n := range groupNames {
				gl[n] = groupVals[i]
			}
		}
		samples = append(samples, observe.MetricSample{
			Timestamp:   bucket.UTC(),
			Value:       value,
			GroupLabels: gl,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &metricStream{samples: samples, idx: -1}, nil
}

// stringifyValue renders an arbitrary driver value as a string for the generic
// RawSQL path.
func stringifyValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", t)
	}
}
