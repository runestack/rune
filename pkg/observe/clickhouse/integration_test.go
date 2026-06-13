//go:build integration

// Integration test for the ClickHouse adapter against a real server spun up via
// testcontainers. It is excluded from the default build/CI (which has no
// ClickHouse and shouldn't pull a Docker image) by the `integration` tag.
//
// Run it with a Docker daemon available:
//
//	go test -tags integration -run TestIntegration -timeout 300s ./pkg/observe/clickhouse/
//
// It exercises the full round-trip the unit tests can't: EnsureSchema (incl. the
// retention TTL), batched Write, log/metric/label queries through the lowered
// SQL, and the Advanced-tier RawSQL path — proving the driver wiring works end
// to end, not just that the generated SQL strings look right.
package clickhouse_test

import (
	"context"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/observe"
	chstore "github.com/runestack/rune/pkg/observe/clickhouse"
	tcch "github.com/testcontainers/testcontainers-go/modules/clickhouse"
)

const clickhouseImage = "clickhouse/clickhouse-server:24.3-alpine"

func startClickHouse(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, err := tcch.Run(ctx, clickhouseImage,
		tcch.WithUsername("default"),
		tcch.WithPassword("clickhouse"),
		tcch.WithDatabase("default"),
	)
	if err != nil {
		t.Fatalf("start clickhouse container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate clickhouse container: %v", err)
		}
	})
	dsn, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

func collectRows(t *testing.T, s observe.LogStore, q *observe.Query) []observe.LogRow {
	t.Helper()
	ctx := context.Background()
	rs, err := s.Execute(ctx, q)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer rs.Close()
	if rs.IsMetric() {
		t.Fatalf("expected log rows, got metric stream")
	}
	var out []observe.LogRow
	for rs.Next(ctx) {
		out = append(out, rs.Row())
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("row stream err: %v", err)
	}
	return out
}

func collectSamples(t *testing.T, s observe.LogStore, q *observe.Query) []observe.MetricSample {
	t.Helper()
	ctx := context.Background()
	rs, err := s.Execute(ctx, q)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer rs.Close()
	if !rs.IsMetric() {
		t.Fatalf("expected metric stream, got log rows")
	}
	var out []observe.MetricSample
	for rs.Next(ctx) {
		out = append(out, rs.Sample())
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("sample stream err: %v", err)
	}
	return out
}

// toggleProxy is a minimal TCP proxy whose forwarding can be switched on and
// off at runtime. With forwarding off it accepts a connection and immediately
// closes it, which breaks ClickHouse's native handshake (connection reset) —
// simulating a backend that is unreachable / mid-startup. With forwarding on it
// pipes bytes to the real backend. This lets a single Store instance see the
// backend go from down to up without recreating it, which is exactly the #104
// scenario.
type toggleProxy struct {
	ln      net.Listener
	backend string
	forward atomic.Bool
}

func newToggleProxy(t *testing.T, backend string) *toggleProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	p := &toggleProxy{ln: ln, backend: backend}
	t.Cleanup(func() { _ = ln.Close() })
	go p.serve()
	return p
}

func (p *toggleProxy) addr() string { return p.ln.Addr().String() }

func (p *toggleProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return // listener closed on cleanup
		}
		go p.handle(c)
	}
}

func (p *toggleProxy) handle(c net.Conn) {
	if !p.forward.Load() {
		_ = c.Close() // refuse: downstream handshake fails fast
		return
	}
	up, err := net.Dial("tcp", p.backend)
	if err != nil {
		_ = c.Close()
		return
	}
	go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
	_, _ = io.Copy(c, up)
	_ = c.Close()
}

// TestIntegration_AutoMigrateRetriesAfterFailure is the regression test for
// #104: if the first AutoMigrate attempt happens while ClickHouse is
// unreachable, the failure must NOT be cached forever. Before the fix the
// sync.Once consumed its slot on the failed attempt and every later Write
// replayed the same frozen schema error until runed restarted; the spool then
// filled and dropped logs. With retry-until-success the Store recovers on its
// own once the backend is healthy, without being recreated.
func TestIntegration_AutoMigrateRetriesAfterFailure(t *testing.T) {
	realDSN := startClickHouse(t)
	ctx := context.Background()

	// Stand a togglable proxy in front of the real server and point the store at
	// the proxy. Forwarding starts OFF, so the backend looks down.
	u, err := url.Parse(realDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	proxy := newToggleProxy(t, u.Host)
	u.Host = proxy.addr()
	proxyDSN := u.String()

	s, err := chstore.New(chstore.Config{
		DSN:         proxyDSN,
		Database:    "runesight",
		Table:       "logs",
		AutoMigrate: true,
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	rec := []observe.LogRecord{{
		Timestamp: time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
		Namespace: "default", Service: "api", Instance: "api-1", Node: "n1",
		Stream: "stdout", Level: "info", Line: "hello",
	}}

	// First write while the backend is "down": schema migration must fail.
	if err := s.Write(ctx, rec); err == nil {
		t.Fatal("expected first write to fail while backend is unreachable, got nil")
	}

	// Backend comes up. The SAME store must recover on a subsequent write.
	proxy.forward.Store(true)

	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = s.Write(ctx, rec); lastErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("store never recovered after backend came up (cached schema error?): %v", lastErr)
	}

	// And the schema/data are genuinely there — not a silently-skipped migration.
	rows := collectRows(t, s, &observe.Query{
		RawSQL: "SELECT line FROM runesight.logs WHERE service = 'api'",
	})
	if len(rows) == 0 {
		t.Fatal("expected the recovered write to be queryable, got 0 rows")
	}
}

func TestIntegration_ClickHouseRoundTrip(t *testing.T) {
	dsn := startClickHouse(t)
	ctx := context.Background()

	s, err := chstore.New(chstore.Config{
		DSN:           dsn,
		Database:      "runesight",
		Table:         "logs",
		AutoMigrate:   true,
		RetentionDays: 30, // exercises the DELETE TTL in the DDL
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	// EnsureSchema creates the database + table (idempotent); Health pings.
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if err := s.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}

	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	recs := []observe.LogRecord{
		{Timestamp: base, Namespace: "default", Service: "api", Instance: "api-1", Node: "n1", Stream: "stdout", Level: "info", Line: "request received", Labels: map[string]string{"app": "web"}},
		{Timestamp: base.Add(1 * time.Second), Namespace: "default", Service: "api", Instance: "api-1", Node: "n1", Stream: "stderr", Level: "error", Line: "boom: handler failed", Labels: map[string]string{"app": "web"}},
		{Timestamp: base.Add(2 * time.Second), Namespace: "default", Service: "api", Instance: "api-2", Node: "n2", Stream: "stdout", Level: "info", Line: "request received"},
		{Timestamp: base.Add(3 * time.Second), Namespace: "default", Service: "web", Instance: "web-1", Node: "n1", Stream: "stdout", Level: "warn", Line: "slow response"},
	}
	if err := s.Write(ctx, recs); err != nil {
		t.Fatalf("write: %v", err)
	}

	window := func(q *observe.Query) *observe.Query {
		q.Start = base.Add(-time.Hour)
		q.End = base.Add(time.Hour)
		return q
	}
	parse := func(logql string) *observe.Query {
		q, perr := observe.ParseLogQL(logql, time.Time{}, time.Time{}, 0, false)
		if perr != nil {
			t.Fatalf("parse %q: %v", logql, perr)
		}
		return window(q)
	}

	t.Run("label match", func(t *testing.T) {
		if rows := collectRows(t, s, parse(`{service="api"}`)); len(rows) != 3 {
			t.Fatalf("want 3 api rows, got %d", len(rows))
		}
	})

	t.Run("line filter", func(t *testing.T) {
		rows := collectRows(t, s, parse(`{service="api"} |= "boom"`))
		if len(rows) != 1 || rows[0].Line != "boom: handler failed" {
			t.Fatalf("want the single boom row, got %+v", rows)
		}
		// The promoted dims must be lifted into the row's Labels.
		if rows[0].Level != "error" || rows[0].Labels["service"] != "api" || rows[0].Labels["app"] != "web" {
			t.Fatalf("row dims/labels wrong: level=%q labels=%+v", rows[0].Level, rows[0].Labels)
		}
	})

	t.Run("custom label match via Map", func(t *testing.T) {
		if rows := collectRows(t, s, parse(`{app="web"}`)); len(rows) != 2 {
			t.Fatalf("want 2 app=web rows, got %d", len(rows))
		}
	})

	t.Run("regex label match", func(t *testing.T) {
		if rows := collectRows(t, s, parse(`{instance=~"api-.*"}`)); len(rows) != 3 {
			t.Fatalf("want 3 api-* rows, got %d", len(rows))
		}
	})

	t.Run("count_over_time sum by level", func(t *testing.T) {
		samples := collectSamples(t, s, parse(`sum by (level) (count_over_time({service="api"}[1h]))`))
		byLevel := map[string]float64{}
		for _, sm := range samples {
			byLevel[sm.GroupLabels["level"]] += sm.Value
		}
		if byLevel["info"] != 2 || byLevel["error"] != 1 {
			t.Fatalf("level counts wrong: %+v", byLevel)
		}
	})

	t.Run("label values ranked by count", func(t *testing.T) {
		vals, lerr := s.Labels(ctx, observe.Selector{Name: "service", Start: base.Add(-time.Hour), End: base.Add(time.Hour)})
		if lerr != nil {
			t.Fatal(lerr)
		}
		if len(vals) != 2 || vals[0].Value != "api" || vals[0].Count != 3 {
			t.Fatalf("want api(3) ranked first, got %+v", vals)
		}
	})

	t.Run("label names include custom keys", func(t *testing.T) {
		names, lerr := s.Labels(ctx, observe.Selector{Start: base.Add(-time.Hour), End: base.Add(time.Hour)})
		if lerr != nil {
			t.Fatal(lerr)
		}
		seen := map[string]bool{}
		for _, n := range names {
			seen[n.Name] = true
		}
		if !seen["service"] || !seen["app"] {
			t.Fatalf("want promoted + custom 'app' in names, got %+v", names)
		}
	})

	t.Run("raw SQL advanced tier", func(t *testing.T) {
		raw := &observe.Query{RawSQL: "SELECT service, count() AS c FROM runesight.logs GROUP BY service ORDER BY c DESC, service ASC"}
		rows := collectRows(t, s, raw)
		if len(rows) != 2 {
			t.Fatalf("want 2 grouped rows, got %d", len(rows))
		}
		if rows[0].Labels["service"] != "api" || rows[0].Labels["c"] != "3" {
			t.Fatalf("raw result wrong: %+v", rows[0].Labels)
		}
	})
}
