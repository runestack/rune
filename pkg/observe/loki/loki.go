// Package loki implements observe.LogStore against a Loki backend (the light
// optional sink). It is Core-tier only: it re-emits the parsed Query AST to
// Loki's HTTP API (it never parses LogQL — the ObserveService does that and
// hands this store the AST, which renderLogQL turns back into a LogQL string)
// and writes via Loki's push API.
//
// Loki is object-storage-native: chunks flush to S3/GCS as the primary durable
// store, so this adapter has no tiering of its own — long retention is a Loki
// deployment concern, not the adapter's (plan §6.4).
package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

// Compile-time assertion that Store satisfies the seam.
var _ observe.LogStore = (*Store)(nil)

// defaultQueryLimit caps returned log lines when a Query sets Limit=0, matching
// the embedded store's default so backends behave alike.
const defaultQueryLimit = 1000

// Config configures the Loki store.
type Config struct {
	// BaseURL is the Loki HTTP endpoint (e.g. http://loki:3100).
	BaseURL string

	// TenantID is sent as the X-Scope-OrgID header for multi-tenant Loki.
	// Empty for single-tenant deployments.
	TenantID string

	// Timeout bounds a single HTTP request to Loki. Zero uses a default.
	Timeout time.Duration

	// HTTPClient overrides the client used for requests. nil uses a default.
	HTTPClient *http.Client
}

// Store is the Loki-backed observe.LogStore.
type Store struct {
	cfg  Config
	http *http.Client
}

// New constructs a Loki store. It does not dial Loki; call Health to verify
// connectivity.
func New(cfg Config) (*Store, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("loki: BaseURL is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &Store{cfg: cfg, http: hc}, nil
}

// Capabilities reports the Loki capability handshake: Core tier only. No raw
// SQL, no percentiles, no cheap high-cardinality filters (cardinality is Loki's
// OOM failure mode).
func (s *Store) Capabilities() observe.Capabilities {
	return observe.Capabilities{
		Backend:                "loki",
		MaxTier:                observe.TierCore,
		RawSQL:                 false,
		Percentiles:            false,
		HighCardinalityFilters: false,
		Parsers:                true,
	}
}

// --- ingest ---

type lokiPushStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"` // [ [ "<unix_ns>", "<line>" ], ... ]
}

type lokiPush struct {
	Streams []lokiPushStream `json:"streams"`
}

// Write persists a batch via Loki's push API. Records are grouped into Loki
// streams keyed by their StreamLabels, and each stream's values are sorted
// ascending by timestamp (Loki rejects out-of-order values within a stream on
// older deployments).
func (s *Store) Write(ctx context.Context, batch []observe.LogRecord) error {
	if len(batch) == 0 {
		return nil
	}
	type stream struct {
		labels map[string]string
		values [][2]string
	}
	streams := map[string]*stream{}
	now := time.Now()
	for _, r := range batch {
		ls := r.StreamLabels()
		key := canonicalLabels(ls)
		st := streams[key]
		if st == nil {
			st = &stream{labels: ls}
			streams[key] = st
		}
		ts := r.Timestamp
		if ts.IsZero() {
			ts = now
		}
		st.values = append(st.values, [2]string{strconv.FormatInt(ts.UnixNano(), 10), r.Line})
	}

	payload := lokiPush{Streams: make([]lokiPushStream, 0, len(streams))}
	for _, st := range streams {
		sort.Slice(st.values, func(i, j int) bool {
			// 19-digit nanos for the current era sort lexically == numerically,
			// but parse to be correct across widths.
			a, _ := strconv.ParseInt(st.values[i][0], 10, 64)
			b, _ := strconv.ParseInt(st.values[j][0], 10, 64)
			return a < b
		})
		payload.Streams = append(payload.Streams, lokiPushStream{Stream: st.labels, Values: st.values})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("loki: marshal push: %w", err)
	}
	resp, err := s.do(ctx, http.MethodPost, "/loki/api/v1/push", nil, bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	return expectOK(resp)
}

// --- query ---

type lokiQueryResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// Execute renders the AST to LogQL and runs it against Loki's query_range
// endpoint, wrapping the streamed JSON in a ResultStream. RawSQL and Advanced-
// tier aggregations (percentiles) are rejected with ErrCapabilityUnsupported.
func (s *Store) Execute(ctx context.Context, q *observe.Query) (observe.ResultStream, error) {
	if q.RawSQL != "" {
		return nil, observe.ErrCapabilityUnsupported
	}
	if q.Aggregation != nil && q.Aggregation.Op == observe.AggQuantileOverTime {
		return nil, observe.ErrCapabilityUnsupported
	}

	logql, err := renderLogQL(q)
	if err != nil {
		return nil, err
	}

	qv := url.Values{}
	qv.Set("query", logql)
	if !q.Start.IsZero() {
		qv.Set("start", strconv.FormatInt(q.Start.UnixNano(), 10))
	}
	if !q.End.IsZero() {
		qv.Set("end", strconv.FormatInt(q.End.UnixNano(), 10))
	}
	if q.IsMetricQuery() {
		qv.Set("step", formatLokiDuration(q.Aggregation.Step))
	} else {
		limit := q.Limit
		if limit <= 0 {
			limit = defaultQueryLimit
		}
		qv.Set("limit", strconv.Itoa(limit))
		if q.Direction == observe.DirectionForward {
			qv.Set("direction", "forward")
		} else {
			qv.Set("direction", "backward")
		}
	}

	resp, err := s.do(ctx, http.MethodGet, "/loki/api/v1/query_range", qv, nil, "")
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	if err := expectOK(resp); err != nil {
		return nil, err
	}

	var lr lokiQueryResp
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("loki: decode query response: %w", err)
	}
	return parseResult(q, &lr)
}

// parseResult turns a Loki query response into a ResultStream: "streams" =>
// log rows (merged across streams, re-sorted by direction, re-limited);
// "matrix" => metric samples.
func parseResult(q *observe.Query, lr *lokiQueryResp) (observe.ResultStream, error) {
	switch lr.Data.ResultType {
	case "streams":
		var streams []struct {
			Stream map[string]string   `json:"stream"`
			Values [][]json.RawMessage `json:"values"`
		}
		if len(lr.Data.Result) > 0 {
			if err := json.Unmarshal(lr.Data.Result, &streams); err != nil {
				return nil, fmt.Errorf("loki: decode streams: %w", err)
			}
		}
		var rows []observe.LogRow
		for _, st := range streams {
			for _, v := range st.Values {
				if len(v) < 2 {
					continue
				}
				var tsStr, line string
				_ = json.Unmarshal(v[0], &tsStr)
				_ = json.Unmarshal(v[1], &line)
				ns, _ := strconv.ParseInt(tsStr, 10, 64)
				rows = append(rows, observe.LogRow{
					Timestamp: time.Unix(0, ns).UTC(),
					Line:      line,
					Stream:    st.Stream["stream"],
					Level:     st.Stream["level"],
					Labels:    st.Stream,
				})
			}
		}
		sortRows(rows, q.Direction)
		if q.Limit > 0 && len(rows) > q.Limit {
			rows = rows[:q.Limit]
		}
		return &resultStream{rows: rows, idx: -1}, nil

	case "matrix":
		var series []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		}
		if len(lr.Data.Result) > 0 {
			if err := json.Unmarshal(lr.Data.Result, &series); err != nil {
				return nil, fmt.Errorf("loki: decode matrix: %w", err)
			}
		}
		var samples []observe.MetricSample
		for _, ser := range series {
			group := ser.Metric
			if len(group) == 0 {
				group = nil
			}
			for _, v := range ser.Values {
				if len(v) < 2 {
					continue
				}
				var tsSec float64
				_ = json.Unmarshal(v[0], &tsSec)
				var valStr string
				_ = json.Unmarshal(v[1], &valStr)
				val, _ := strconv.ParseFloat(valStr, 64)
				samples = append(samples, observe.MetricSample{
					Timestamp:   time.Unix(0, int64(tsSec*1e9)).UTC(),
					Value:       val,
					GroupLabels: group,
				})
			}
		}
		return &resultStream{metric: true, samples: samples, idx: -1}, nil

	case "":
		// No data — return an empty stream of the expected shape.
		return &resultStream{metric: q.IsMetricQuery(), idx: -1}, nil

	default:
		return nil, fmt.Errorf("loki: unexpected resultType %q", lr.Data.ResultType)
	}
}

// --- labels ---

// Labels enumerates label names (sel.Name=="") or the values of a named
// dimension via Loki's label API. Loki does not return occurrence counts, so
// LabelValue.Count stays 0.
func (s *Store) Labels(ctx context.Context, sel observe.Selector) ([]observe.LabelValue, error) {
	qv := url.Values{}
	if !sel.Start.IsZero() {
		qv.Set("start", strconv.FormatInt(sel.Start.UnixNano(), 10))
	}
	if !sel.End.IsZero() {
		qv.Set("end", strconv.FormatInt(sel.End.UnixNano(), 10))
	}
	if len(sel.Match) > 0 {
		qv.Set("query", renderSelectors(sel.Match))
	}

	path := "/loki/api/v1/labels"
	if sel.Name != "" {
		path = "/loki/api/v1/label/" + url.PathEscape(sel.Name) + "/values"
	}

	resp, err := s.do(ctx, http.MethodGet, path, qv, nil, "")
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	if err := expectOK(resp); err != nil {
		return nil, err
	}

	var lr struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("loki: decode labels: %w", err)
	}

	out := make([]observe.LabelValue, 0, len(lr.Data))
	for _, v := range lr.Data {
		if sel.Name == "" {
			out = append(out, observe.LabelValue{Name: v}) // enumerate names
		} else {
			out = append(out, observe.LabelValue{Name: sel.Name, Value: v})
		}
	}
	if sel.Limit > 0 && len(out) > sel.Limit {
		out = out[:sel.Limit]
	}
	return out, nil
}

// Health probes Loki readiness via GET /ready.
func (s *Store) Health(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodGet, "/ready", nil, nil, "")
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	return expectOK(resp)
}

// --- HTTP plumbing ---

func (s *Store) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := strings.TrimRight(s.cfg.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("loki: build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if s.cfg.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", s.cfg.TenantID)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// expectOK returns an error for any non-2xx response, including a snippet of the
// body. 4xx are caller/permanent errors; 5xx are retryable — both surface here
// with the status code so the caller can decide.
func expectOK(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("loki: %s returned %d: %s", resp.Request.URL.Path, resp.StatusCode, strings.TrimSpace(string(b)))
}

func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<16))
	_ = rc.Close()
}

func sortRows(rows []observe.LogRow, dir observe.Direction) {
	sort.SliceStable(rows, func(i, j int) bool {
		if dir == observe.DirectionForward {
			return rows[i].Timestamp.Before(rows[j].Timestamp)
		}
		return rows[i].Timestamp.After(rows[j].Timestamp)
	})
}

// canonicalLabels renders a label set as a deterministic key for grouping
// records into Loki streams.
func canonicalLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte(0)
	}
	return b.String()
}

// --- result stream ---

// resultStream replays a fully-buffered Loki response (rows or samples). Loki
// responses are small enough to materialise; streaming the HTTP body lazily
// would complicate Close/Err for no real memory win at Core scale.
type resultStream struct {
	metric  bool
	rows    []observe.LogRow
	samples []observe.MetricSample
	idx     int
}

func (s *resultStream) IsMetric() bool { return s.metric }

func (s *resultStream) Next(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	n := len(s.rows)
	if s.metric {
		n = len(s.samples)
	}
	if s.idx+1 >= n {
		s.idx = n
		return false
	}
	s.idx++
	return true
}

func (s *resultStream) Row() observe.LogRow {
	if !s.metric && s.idx >= 0 && s.idx < len(s.rows) {
		return s.rows[s.idx]
	}
	return observe.LogRow{}
}

func (s *resultStream) Sample() observe.MetricSample {
	if s.metric && s.idx >= 0 && s.idx < len(s.samples) {
		return s.samples[s.idx]
	}
	return observe.MetricSample{}
}

func (s *resultStream) Err() error   { return nil }
func (s *resultStream) Close() error { return nil }
