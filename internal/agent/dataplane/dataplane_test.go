package dataplane

import (
	"context"
	"io"
	"math/rand"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/endpoints"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"github.com/runestack/rune/pkg/types"
)

// newTestOlog returns an in-process OrderedLog backed by a temp Badger.
func newTestOlog(t *testing.T) *orderedlog.BadgerBackend {
	t.Helper()
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(dir, "olog")).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	be := orderedlog.NewBadgerBackend(db, orderedlog.BackendOptions{
		Logger: log.GetDefaultLogger().WithComponent("test.olog"),
	})
	if err := be.Open(); err != nil {
		t.Fatalf("olog open: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

func TestCacheSetGetDelete(t *testing.T) {
	c := newCache()
	c.Set("svc", []types.Endpoint{{IP: "1.1.1.1", Port: 80, Healthy: true}})
	got, ok := c.Get("svc")
	if !ok || len(got) != 1 {
		t.Fatalf("Get after Set returned %v ok=%v", got, ok)
	}
	// mutating the returned slice must not affect the cache
	got[0].IP = "2.2.2.2"
	got2, _ := c.Get("svc")
	if got2[0].IP != "1.1.1.1" {
		t.Fatalf("cache returned mutable view: %v", got2)
	}
	c.Delete("svc")
	if _, ok := c.Get("svc"); ok {
		t.Fatal("Get after Delete should be ok=false")
	}
}

func TestCacheHealthyFilters(t *testing.T) {
	c := newCache()
	c.Set("svc", []types.Endpoint{
		{IP: "1.1.1.1", Port: 80, Healthy: true},
		{IP: "2.2.2.2", Port: 80, Healthy: false},
	})
	healthy, ok := c.Healthy("svc")
	if !ok || len(healthy) != 1 || healthy[0].IP != "1.1.1.1" {
		t.Fatalf("Healthy mismatch: %v", healthy)
	}
}

func TestSelectEndpointLocalityModes(t *testing.T) {
	eps := []types.Endpoint{
		{IP: "10.0.0.1", Healthy: true, Metadata: map[string]string{metaNodeID: "nodeA"}},
		{IP: "10.0.0.2", Healthy: true, Metadata: map[string]string{metaNodeID: "nodeB"}},
	}
	rng := rand.New(rand.NewSource(1))

	// none: returns one of the two
	got, ok := selectEndpoint(eps, LocalityNone, "nodeA", rng)
	if !ok {
		t.Fatal("none: ok=false")
	}
	if got.IP != "10.0.0.1" && got.IP != "10.0.0.2" {
		t.Errorf("none: unexpected pick %v", got.IP)
	}

	// local-only on nodeA: must return nodeA's endpoint
	got, ok = selectEndpoint(eps, LocalityLocalOnly, "nodeA", rng)
	if !ok || got.IP != "10.0.0.1" {
		t.Errorf("local-only: expected nodeA, got %+v ok=%v", got, ok)
	}

	// local-only on nodeC: must return ok=false
	if _, ok := selectEndpoint(eps, LocalityLocalOnly, "nodeC", rng); ok {
		t.Error("local-only on missing node should fail")
	}

	// prefer-local on nodeC: degrades to any healthy
	got, ok = selectEndpoint(eps, LocalityPreferLocal, "nodeC", rng)
	if !ok {
		t.Error("prefer-local should fall back when no local")
	}
}

func TestHydrateLoadsExistingEndpoints(t *testing.T) {
	ol := newTestOlog(t)
	if err := endpoints.Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	pub := endpoints.NewPublisher(ol)
	if err := pub.Update(context.Background(), "svc-h", []types.Endpoint{{IP: "10.0.0.1", Port: 1, Healthy: true}}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	dp, err := New(Config{OrderedLog: ol, Mode: ModeDev})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dp.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = dp.Stop(stopCtx)
	}()
	got, ok := dp.Cache().Get("svc-h")
	if !ok || len(got) != 1 || got[0].IP != "10.0.0.1" {
		t.Fatalf("hydrate missed: %+v ok=%v", got, ok)
	}
}

func TestWatchAppliesLiveUpdates(t *testing.T) {
	ol := newTestOlog(t)
	if err := endpoints.Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	dp, err := New(Config{OrderedLog: ol, Mode: ModeDev})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dp.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = dp.Stop(stopCtx)
	}()

	pub := endpoints.NewPublisher(ol)
	if err := pub.Update(ctx, "svc-l", []types.Endpoint{{IP: "10.0.0.7", Port: 80, Healthy: true}}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Wait briefly for the watch loop to deliver.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := dp.Cache().Get("svc-l"); ok && len(got) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watch never delivered svc-l")
}

func TestProxyTCPEcho(t *testing.T) {
	// Stand up a real tcp echo server on a random port.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	echoAddr := echo.Addr().(*net.TCPAddr)

	// Build a dataplane (dev mode -> 127.0.0.1 binding, no nftables).
	ol := newTestOlog(t)
	if err := endpoints.Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	dp, err := New(Config{OrderedLog: ol, Mode: ModeDev})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dp.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = dp.Stop(stopCtx)
	}()

	// Pick a free proxy port.
	proxyPort := mustFreePort(t)
	svc := &types.Service{
		ID: "svc-echo",
		Ports: []types.ServicePort{
			{Name: "tcp", Port: proxyPort, TargetPort: echoAddr.Port, Protocol: "tcp"},
		},
		Discovery: &types.ServiceDiscovery{VIP: "ignored-in-dev"},
	}
	if err := dp.RegisterService(svc); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	// Inject endpoint pointing at the echo server.
	pub := endpoints.NewPublisher(ol)
	if err := pub.Update(ctx, "svc-echo", []types.Endpoint{{
		IP: "127.0.0.1", Port: echoAddr.Port, Healthy: true,
	}}); err != nil {
		t.Fatalf("publish endpoints: %v", err)
	}

	// Wait for the cache to learn the endpoint.
	if !waitForCache(dp, "svc-echo", 1, 2*time.Second) {
		t.Fatal("cache did not learn endpoints")
	}

	// Dial through the proxy.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(proxyPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("expected ping, got %q", got)
	}
}

func TestProxyFailsClosedWhenStale(t *testing.T) {
	ol := newTestOlog(t)
	if err := endpoints.Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	dp, err := New(Config{
		OrderedLog:  ol,
		Mode:        ModeDev,
		StaleBudget: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dp.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = dp.Stop(stopCtx)
	}()

	// Publish an endpoint, then immediately open a listener.
	pub := endpoints.NewPublisher(ol)
	if err := pub.Update(ctx, "svc-stale", []types.Endpoint{{IP: "127.0.0.1", Port: 1, Healthy: true}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !waitForCache(dp, "svc-stale", 1, 2*time.Second) {
		t.Fatal("cache did not learn endpoints")
	}
	proxyPort := mustFreePort(t)
	svc := &types.Service{
		ID:        "svc-stale",
		Ports:     []types.ServicePort{{Port: proxyPort, TargetPort: 1, Protocol: "tcp"}},
		Discovery: &types.ServiceDiscovery{VIP: "ignored"},
	}
	if err := dp.RegisterService(svc); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	// Force the dataplane stale by overwriting lastEvent to long ago.
	dp.lastEventMu.Lock()
	dp.lastEvent = time.Now().Add(-time.Hour)
	dp.lastEventMu.Unlock()

	// New connection should be accepted-then-closed without forwarding.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(proxyPort)), 1*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	// Send some data and expect EOF from the other side.
	_, _ = conn.Write([]byte("data"))
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("stale proxy returned data: %q", buf[:n])
	}
}

// helpers ---------------------------------------------------------------

func waitForCache(dp *Subsystem, svc string, want int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		eps, ok := dp.Cache().Get(svc)
		if ok && len(eps) == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func mustFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func itoa(i int) string {
	// avoid strconv import noise in test
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
