package embedded

import (
	"context"
	"sort"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// rowStream replays a pre-filtered, ordered slice of records as log rows.
type rowStream struct {
	rows []observe.LogRow
	idx  int
	cur  observe.LogRow
}

func newRowStream(records []observe.LogRecord, q *observe.Query) *rowStream {
	// Order: backward => newest first (default), forward => oldest first.
	sort.SliceStable(records, func(i, j int) bool {
		if q.Direction == observe.DirectionForward {
			return records[i].Timestamp.Before(records[j].Timestamp)
		}
		return records[i].Timestamp.After(records[j].Timestamp)
	})

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultQueryLimit
	}
	if len(records) > limit {
		records = records[:limit]
	}

	rows := make([]observe.LogRow, 0, len(records))
	for _, r := range records {
		rows = append(rows, observe.LogRow{
			Timestamp: r.Timestamp,
			Line:      r.Line,
			Stream:    r.Stream,
			Level:     r.Level,
			Labels:    r.StreamLabels(),
		})
	}
	return &rowStream{rows: rows, idx: -1}
}

func (s *rowStream) IsMetric() bool { return false }

func (s *rowStream) Next(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	s.idx++
	if s.idx >= len(s.rows) {
		return false
	}
	s.cur = s.rows[s.idx]
	return true
}

func (s *rowStream) Row() observe.LogRow          { return s.cur }
func (s *rowStream) Sample() observe.MetricSample { return observe.MetricSample{} }
func (s *rowStream) Err() error                   { return nil }
func (s *rowStream) Close() error                 { return nil }

// metricStream computes a count/rate/bytes histogram bucketed by Step and
// optionally grouped by a label dimension, then replays the samples.
type metricStream struct {
	samples []observe.MetricSample
	idx     int
	cur     observe.MetricSample
}

func newMetricStream(records []observe.LogRecord, agg *observe.Aggregation) *metricStream {
	step := agg.Step
	if step <= 0 {
		step = time.Minute
	}

	// bucket key = (bucketStartUnixNano, groupKey). We keep both the numeric
	// aggregate and the resolved group labels for output.
	type bucketKey struct {
		bucket int64
		group  string
	}
	type acc struct {
		ts     time.Time
		count  float64
		bytes  float64
		labels map[string]string
	}
	buckets := map[bucketKey]*acc{}

	for _, r := range records {
		bstart := r.Timestamp.Truncate(step)
		groupLabels := resolveGroup(r, agg.GroupBy)
		key := bucketKey{bucket: bstart.UnixNano(), group: groupKeyString(groupLabels)}
		a := buckets[key]
		if a == nil {
			a = &acc{ts: bstart, labels: groupLabels}
			buckets[key] = a
		}
		a.count++
		a.bytes += float64(len(r.Line))
	}

	samples := make([]observe.MetricSample, 0, len(buckets))
	for _, a := range buckets {
		var v float64
		switch agg.Op {
		case observe.AggRateOverTime:
			v = a.count / step.Seconds()
		case observe.AggBytesOverTime:
			v = a.bytes
		default: // AggCountOverTime
			v = a.count
		}
		samples = append(samples, observe.MetricSample{
			Timestamp:   a.ts,
			Value:       v,
			GroupLabels: a.labels,
		})
	}
	// Stable order: by timestamp asc, then group key asc.
	sort.Slice(samples, func(i, j int) bool {
		if !samples[i].Timestamp.Equal(samples[j].Timestamp) {
			return samples[i].Timestamp.Before(samples[j].Timestamp)
		}
		return groupKeyString(samples[i].GroupLabels) < groupKeyString(samples[j].GroupLabels)
	})
	return &metricStream{samples: samples, idx: -1}
}

func (s *metricStream) IsMetric() bool { return true }

func (s *metricStream) Next(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	s.idx++
	if s.idx >= len(s.samples) {
		return false
	}
	s.cur = s.samples[s.idx]
	return true
}

func (s *metricStream) Row() observe.LogRow          { return observe.LogRow{} }
func (s *metricStream) Sample() observe.MetricSample { return s.cur }
func (s *metricStream) Err() error                   { return nil }
func (s *metricStream) Close() error                 { return nil }

func resolveGroup(r observe.LogRecord, groupBy []string) map[string]string {
	if len(groupBy) == 0 {
		return nil
	}
	out := make(map[string]string, len(groupBy))
	for _, g := range groupBy {
		if v, ok := recordLabel(r, g); ok {
			out[g] = v
		} else {
			out[g] = ""
		}
	}
	return out
}

// groupKeyString renders a group-labels map deterministically for use as a map
// key and for stable sort ordering.
func groupKeyString(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, m[k]...)
		b = append(b, ',')
	}
	return string(b)
}
