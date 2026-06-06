package observe_test

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/observe/embedded"
)

// The conformance corpus is the contract every Core-tier store must satisfy.
// It is run here against the embedded store (the default backend). When the
// Loki / ClickHouse stores graduate from skeletons, point runConformance at
// them too so all Core backends are held to the same behaviour. Advanced-tier
// cases (RawSQL, percentiles) are intentionally excluded — those are
// ClickHouse-only and get their own suite.
//
// Each case parses a LogQL string into the AST and asserts on Execute results
// over a fixed corpus, exercising: label match (=, !=, =~), line filters
// (|=, !=), count_over_time, rate, and `sum by (level)` grouping.

func corpus(base time.Time) []observe.LogRecord {
	return []observe.LogRecord{
		{Timestamp: base, Service: "api", Namespace: "default", Instance: "api-1", Node: "n1", Stream: "stdout", Level: "info", Line: "request received"},
		{Timestamp: base.Add(1 * time.Second), Service: "api", Namespace: "default", Instance: "api-1", Node: "n1", Stream: "stderr", Level: "error", Line: "boom: handler failed"},
		{Timestamp: base.Add(2 * time.Second), Service: "api", Namespace: "default", Instance: "api-2", Node: "n2", Stream: "stdout", Level: "info", Line: "request received"},
		{Timestamp: base.Add(3 * time.Second), Service: "web", Namespace: "default", Instance: "web-1", Node: "n1", Stream: "stdout", Level: "warn", Line: "slow response"},
		{Timestamp: base.Add(70 * time.Second), Service: "api", Namespace: "default", Instance: "api-1", Node: "n1", Stream: "stderr", Level: "error", Line: "boom: retry exhausted"},
	}
}

func newCoreStore(t *testing.T, base time.Time) observe.LogStore {
	t.Helper()
	s := embedded.New(embedded.Config{Retention: -1})
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(context.Background(), corpus(base)); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	return s
}

func collectRows(t *testing.T, store observe.LogStore, q *observe.Query) []observe.LogRow {
	t.Helper()
	rs, err := store.Execute(context.Background(), q)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer rs.Close()
	if rs.IsMetric() {
		t.Fatalf("expected log rows, got metric stream")
	}
	var out []observe.LogRow
	for rs.Next(context.Background()) {
		out = append(out, rs.Row())
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	return out
}

func collectSamples(t *testing.T, store observe.LogStore, q *observe.Query) []observe.MetricSample {
	t.Helper()
	rs, err := store.Execute(context.Background(), q)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer rs.Close()
	if !rs.IsMetric() {
		t.Fatalf("expected metric stream, got log rows")
	}
	var out []observe.MetricSample
	for rs.Next(context.Background()) {
		out = append(out, rs.Sample())
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	return out
}

func TestConformance_Core(t *testing.T) {
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	store := newCoreStore(t, base)
	window := func(q *observe.Query) *observe.Query {
		q.Start = base.Add(-time.Hour)
		q.End = base.Add(time.Hour)
		return q
	}
	parse := func(t *testing.T, logql string) *observe.Query {
		t.Helper()
		q, err := observe.ParseLogQL(logql, time.Time{}, time.Time{}, 0, false)
		if err != nil {
			t.Fatalf("parse %q: %v", logql, err)
		}
		return window(q)
	}

	t.Run("label equal match", func(t *testing.T) {
		rows := collectRows(t, store, parse(t, `{service="api"}`))
		if len(rows) != 4 {
			t.Fatalf("want 4 api rows, got %d", len(rows))
		}
	})

	t.Run("label not-equal match", func(t *testing.T) {
		rows := collectRows(t, store, parse(t, `{service!="api"}`))
		if len(rows) != 1 {
			t.Fatalf("want 1 non-api row, got %d", len(rows))
		}
	})

	t.Run("label regex match", func(t *testing.T) {
		rows := collectRows(t, store, parse(t, `{instance=~"api-.*"}`))
		if len(rows) != 4 {
			t.Fatalf("want 4 instance-regex rows, got %d", len(rows))
		}
	})

	t.Run("line contains filter", func(t *testing.T) {
		rows := collectRows(t, store, parse(t, `{service="api"} |= "boom"`))
		if len(rows) != 2 {
			t.Fatalf("want 2 boom rows, got %d", len(rows))
		}
	})

	t.Run("line not-contains filter", func(t *testing.T) {
		rows := collectRows(t, store, parse(t, `{service="api"} != "boom"`))
		if len(rows) != 2 {
			t.Fatalf("want 2 non-boom rows, got %d", len(rows))
		}
	})

	t.Run("count_over_time", func(t *testing.T) {
		samples := collectSamples(t, store, parse(t, `count_over_time({service="api"}[1m])`))
		var total float64
		for _, s := range samples {
			total += s.Value
		}
		if total != 4 {
			t.Fatalf("want total count 4 across api buckets, got %v", total)
		}
		// The 70s-late record falls in a second bucket.
		if len(samples) != 2 {
			t.Fatalf("want 2 time buckets, got %d", len(samples))
		}
	})

	t.Run("rate", func(t *testing.T) {
		samples := collectSamples(t, store, parse(t, `rate({service="web"}[1m])`))
		if len(samples) != 1 {
			t.Fatalf("want 1 web bucket, got %d", len(samples))
		}
		// One line over a 60s window => 1/60 per second.
		if got := samples[0].Value; got < 0.016 || got > 0.017 {
			t.Fatalf("want ~0.0166 rate, got %v", got)
		}
	})

	t.Run("sum by level grouping", func(t *testing.T) {
		samples := collectSamples(t, store, parse(t, `sum by (level) (count_over_time({service="api"}[1h]))`))
		byLevel := map[string]float64{}
		for _, s := range samples {
			byLevel[s.GroupLabels["level"]] += s.Value
		}
		if byLevel["error"] != 2 {
			t.Fatalf("want 2 error lines, got %v", byLevel["error"])
		}
		if byLevel["info"] != 2 {
			t.Fatalf("want 2 info lines, got %v", byLevel["info"])
		}
	})

	t.Run("advanced tier rejected", func(t *testing.T) {
		q := window(&observe.Query{
			Selectors: []observe.Matcher{{Label: "service", Op: observe.MatchEqual, Value: "api"}},
			RawSQL:    "SELECT 1",
		})
		_, err := store.Execute(context.Background(), q)
		if err == nil {
			t.Fatal("want ErrCapabilityUnsupported for RawSQL on Core store")
		}
	})
}

func TestParseLogQL_Rejects(t *testing.T) {
	cases := []string{
		``,
		`no selector`,
		`{service="api"`,
		`quantile_over_time(0.99, {service="api"}[5m])`, // advanced tier
	}
	for _, c := range cases {
		if _, err := observe.ParseLogQL(c, time.Time{}, time.Time{}, 0, false); err == nil {
			t.Errorf("ParseLogQL(%q) = nil error, want error", c)
		}
	}
}
