package loki

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

func newTestStore(t *testing.T, h http.HandlerFunc) (*Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, err := New(Config{BaseURL: srv.URL, TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, srv
}

func TestWrite_GroupsStreamsAndPushes(t *testing.T) {
	var got lokiPush
	var sawTenant string
	s, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/push" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("want json content-type, got %q", ct)
		}
		sawTenant = r.Header.Get("X-Scope-OrgID")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode push body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	err := s.Write(context.Background(), []observe.LogRecord{
		// out of order on purpose; Write must sort within a stream.
		{Timestamp: base.Add(2 * time.Second), Service: "api", Namespace: "default", Instance: "api-1", Node: "n1", Line: "second"},
		{Timestamp: base, Service: "api", Namespace: "default", Instance: "api-1", Node: "n1", Line: "first"},
		{Timestamp: base, Service: "web", Namespace: "default", Instance: "web-1", Node: "n1", Line: "other"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sawTenant != "tenant-a" {
		t.Errorf("tenant header not propagated, got %q", sawTenant)
	}
	if len(got.Streams) != 2 {
		t.Fatalf("want 2 streams (api, web), got %d", len(got.Streams))
	}
	// Find the api stream and assert its values are ts-ascending.
	for _, st := range got.Streams {
		if st.Stream["service"] != "api" {
			continue
		}
		if len(st.Values) != 2 {
			t.Fatalf("want 2 api values, got %d", len(st.Values))
		}
		if st.Values[0][1] != "first" || st.Values[1][1] != "second" {
			t.Fatalf("api values not sorted ascending: %+v", st.Values)
		}
		wantTs := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC).UnixNano()
		if st.Values[0][0] != itoa(wantTs) {
			t.Errorf("first ts = %q, want %d", st.Values[0][0], wantTs)
		}
	}
}

func TestExecute_LogQuery(t *testing.T) {
	var sawQuery, sawDirection, sawLimit string
	s, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		sawQuery = r.URL.Query().Get("query")
		sawDirection = r.URL.Query().Get("direction")
		sawLimit = r.URL.Query().Get("limit")
		writeJSON(w, map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "streams",
				"result": []any{
					map[string]any{
						"stream": map[string]string{"service": "api", "level": "error", "stream": "stderr"},
						"values": [][2]string{
							{"1717675200000000000", "boom"},       // 12:00:00
							{"1717675202000000000", "boom retry"}, // 12:00:02
						},
					},
				},
			},
		})
	})

	q, err := observe.ParseLogQL(`{service="api"} |= "boom"`, time.Time{}, time.Time{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := s.Execute(context.Background(), q)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer rs.Close()
	if rs.IsMetric() {
		t.Fatal("want log rows, got metric stream")
	}

	var rows []observe.LogRow
	for rs.Next(context.Background()) {
		rows = append(rows, rs.Row())
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// Default direction is backward => newest first.
	if rows[0].Line != "boom retry" || rows[1].Line != "boom" {
		t.Fatalf("rows not newest-first: %+v", rows)
	}
	if rows[0].Level != "error" || rows[0].Stream != "stderr" {
		t.Errorf("derived dims wrong: level=%q stream=%q", rows[0].Level, rows[0].Stream)
	}
	// The AST must have been rendered back into LogQL on the wire.
	if sawQuery != `{service="api"} |= "boom"` {
		t.Errorf("rendered query = %q", sawQuery)
	}
	if sawDirection != "backward" {
		t.Errorf("direction = %q, want backward", sawDirection)
	}
	if sawLimit != "1000" {
		t.Errorf("limit = %q, want default 1000", sawLimit)
	}
}

func TestExecute_MetricQuery(t *testing.T) {
	var sawQuery, sawStep string
	s, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.Query().Get("query")
		sawStep = r.URL.Query().Get("step")
		writeJSON(w, map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "matrix",
				"result": []any{
					map[string]any{
						"metric": map[string]string{"level": "error"},
						"values": []any{
							[]any{1717675200.0, "2"},
							[]any{1717675260.0, "1"},
						},
					},
				},
			},
		})
	})

	q, err := observe.ParseLogQL(`sum by (level) (count_over_time({service="api"}[1m]))`, time.Time{}, time.Time{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := s.Execute(context.Background(), q)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer rs.Close()
	if !rs.IsMetric() {
		t.Fatal("want metric stream")
	}
	var total float64
	var n int
	for rs.Next(context.Background()) {
		smp := rs.Sample()
		if smp.GroupLabels["level"] != "error" {
			t.Errorf("group label lost: %+v", smp.GroupLabels)
		}
		total += smp.Value
		n++
	}
	if n != 2 || total != 3 {
		t.Fatalf("want 2 samples totalling 3, got n=%d total=%v", n, total)
	}
	if sawQuery != `sum by (level) (count_over_time({service="api"} [60s]))` {
		t.Errorf("rendered metric query = %q", sawQuery)
	}
	if sawStep != "60s" {
		t.Errorf("step = %q, want 60s", sawStep)
	}
}

func TestExecute_RejectsAdvancedTier(t *testing.T) {
	s, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be hit for an advanced-tier query")
	})
	q := &observe.Query{
		Selectors: []observe.Matcher{{Label: "service", Op: observe.MatchEqual, Value: "api"}},
		RawSQL:    "SELECT 1",
	}
	if _, err := s.Execute(context.Background(), q); err != observe.ErrCapabilityUnsupported {
		t.Fatalf("want ErrCapabilityUnsupported, got %v", err)
	}
	// Percentile aggregation is also Advanced tier.
	q2 := &observe.Query{
		Selectors:   []observe.Matcher{{Label: "service", Op: observe.MatchEqual, Value: "api"}},
		Aggregation: &observe.Aggregation{Op: observe.AggQuantileOverTime, Step: time.Minute, Quantile: 0.99, Field: "dur"},
	}
	if _, err := s.Execute(context.Background(), q2); err != observe.ErrCapabilityUnsupported {
		t.Fatalf("want ErrCapabilityUnsupported for quantile, got %v", err)
	}
}

func TestLabels_NamesAndValues(t *testing.T) {
	s, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/labels":
			writeJSON(w, map[string]any{"status": "success", "data": []string{"service", "namespace"}})
		case "/loki/api/v1/label/service/values":
			if got := r.URL.Query().Get("query"); got != `{namespace="default"}` {
				t.Errorf("match constraint not passed, got %q", got)
			}
			writeJSON(w, map[string]any{"status": "success", "data": []string{"api", "web"}})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})

	names, err := s.Labels(context.Background(), observe.Selector{})
	if err != nil {
		t.Fatalf("Labels(names): %v", err)
	}
	if len(names) != 2 || names[0].Name != "service" {
		t.Fatalf("want [service namespace] names, got %+v", names)
	}

	vals, err := s.Labels(context.Background(), observe.Selector{
		Name:  "service",
		Match: []observe.Matcher{{Label: "namespace", Op: observe.MatchEqual, Value: "default"}},
	})
	if err != nil {
		t.Fatalf("Labels(values): %v", err)
	}
	if len(vals) != 2 || vals[0].Name != "service" || vals[0].Value != "api" {
		t.Fatalf("want service=api,web, got %+v", vals)
	}
}

func TestHealth(t *testing.T) {
	ready := true
	s, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if ready {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if err := s.Health(context.Background()); err != nil {
		t.Fatalf("Health ready: %v", err)
	}
	ready = false
	if err := s.Health(context.Background()); err == nil {
		t.Fatal("want error when Loki not ready")
	}
}

func TestExecute_SurfacesServerError(t *testing.T) {
	s, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("rpc error"))
	})
	q, _ := observe.ParseLogQL(`{service="api"}`, time.Time{}, time.Time{}, 0, false)
	if _, err := s.Execute(context.Background(), q); err == nil {
		t.Fatal("want error on 500 from Loki")
	}
}

// --- renderer unit tests ---

func TestRenderLogQL(t *testing.T) {
	cases := []struct {
		name  string
		logql string
		want  string
	}{
		{"selector", `{service="api"}`, `{service="api"}`},
		{"multi matcher + ops", `{service="api", instance=~"api-.*"}`, `{service="api", instance=~"api-.*"}`},
		{"line filters", `{service="api"} |= "boom" != "noise"`, `{service="api"} |= "boom" != "noise"`},
		{"count_over_time", `count_over_time({service="api"}[5m])`, `count_over_time({service="api"} [300s])`},
		{"rate", `rate({service="web"}[1m])`, `rate({service="web"} [60s])`},
		{"sum by", `sum by (level) (count_over_time({service="api"}[1h]))`, `sum by (level) (count_over_time({service="api"} [3600s]))`},
		{"filter inside range", `count_over_time({service="api"} |= "boom" [5m])`, `count_over_time({service="api"} |= "boom" [300s])`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := observe.ParseLogQL(c.logql, time.Time{}, time.Time{}, 0, false)
			if err != nil {
				t.Fatalf("parse %q: %v", c.logql, err)
			}
			got, err := renderLogQL(q)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != c.want {
				t.Errorf("render(%q) = %q, want %q", c.logql, got, c.want)
			}
		})
	}
}

func TestFormatLokiDuration(t *testing.T) {
	cases := map[time.Duration]string{
		time.Minute:             "60s",
		time.Hour:               "3600s",
		30 * time.Second:        "30s",
		1500 * time.Millisecond: "1500ms",
		0:                       "1m",
	}
	for d, want := range cases {
		if got := formatLokiDuration(d); got != want {
			t.Errorf("formatLokiDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
