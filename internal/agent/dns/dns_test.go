package dns

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
	"github.com/runestack/rune/pkg/log"
)

// fakeZone implements ZoneProvider for tests.
type fakeZone struct {
	mu      sync.RWMutex
	records map[string][]net.IP // key: "<ns>/<name>"
}

func (f *fakeZone) LookupA(ns, name string) ([]net.IP, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	ips, ok := f.records[ns+"/"+name]
	if !ok {
		return nil, false
	}
	out := make([]net.IP, len(ips))
	copy(out, ips)
	return out, true
}

func (f *fakeZone) set(ns, name string, ips ...net.IP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = map[string][]net.IP{}
	}
	f.records[ns+"/"+name] = ips
}

type toggleFresh struct{ fresh atomic.Bool }

func (t *toggleFresh) IsFresh() bool { return t.fresh.Load() }

// freePort finds an open UDP+TCP loopback port for tests.
func freePort(t *testing.T) string {
	t.Helper()
	// UDP probe; the same port is almost always free for TCP on
	// loopback, which is good enough for unit tests.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp probe: %v", err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

func startServer(t *testing.T, cfg Config) (*Subsystem, string) {
	t.Helper()
	if len(cfg.BindAddrs) == 0 {
		cfg.BindAddrs = []string{freePort(t)}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger()
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Stop(context.Background())
	})
	// Poll with a real DNS query until the server answers (any rcode
	// counts; we only need to know the listener is bound).
	probe := &mdns.Client{Net: "udp", Timeout: 100 * time.Millisecond}
	probeMsg := new(mdns.Msg)
	probeMsg.SetQuestion("ready.probe.", mdns.TypeA)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := probe.Exchange(probeMsg, cfg.BindAddrs[0]); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return s, cfg.BindAddrs[0]
}

func query(t *testing.T, addr, name string, qtype uint16) *mdns.Msg {
	t.Helper()
	c := &mdns.Client{Net: "udp", Timeout: 2 * time.Second}
	m := new(mdns.Msg)
	m.SetQuestion(mdns.Fqdn(name), qtype)
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("exchange %s: %v", name, err)
	}
	return resp
}

func TestZoneA_Resolves(t *testing.T) {
	z := &fakeZone{}
	z.set("prod", "web", net.IPv4(10, 96, 0, 5))
	upstream := func() []string { return []string{"127.0.0.1:1"} } // unused for in-zone
	_, addr := startServer(t, Config{
		Zone:             z,
		UpstreamProvider: upstream,
	})

	resp := query(t, addr, "web.prod.rune", mdns.TypeA)
	if resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("rcode = %d, want NOERROR", resp.Rcode)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("want 1 answer, got %d", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*mdns.A)
	if !ok {
		t.Fatalf("answer not A: %T", resp.Answer[0])
	}
	if !a.A.Equal(net.IPv4(10, 96, 0, 5)) {
		t.Errorf("got IP %s", a.A)
	}
	if a.Hdr.Ttl != DefaultTTL {
		t.Errorf("TTL = %d", a.Hdr.Ttl)
	}
	if !resp.Authoritative {
		t.Error("expected AA flag")
	}
}

func TestZoneA_NXDOMAIN(t *testing.T) {
	z := &fakeZone{}
	upstream := func() []string { return []string{"127.0.0.1:1"} }
	_, addr := startServer(t, Config{
		Zone:             z,
		UpstreamProvider: upstream,
	})
	resp := query(t, addr, "missing.prod.rune", mdns.TypeA)
	if resp.Rcode != mdns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", resp.Rcode)
	}
}

func TestZoneA_StaleReturnsServfail(t *testing.T) {
	z := &fakeZone{}
	z.set("prod", "web", net.IPv4(10, 96, 0, 5))
	tg := &toggleFresh{}
	tg.fresh.Store(false)
	upstream := func() []string { return []string{"127.0.0.1:1"} }
	_, addr := startServer(t, Config{
		Zone:             z,
		Freshness:        tg,
		UpstreamProvider: upstream,
	})
	resp := query(t, addr, "web.prod.rune", mdns.TypeA)
	if resp.Rcode != mdns.RcodeServerFailure {
		t.Fatalf("stale rcode = %d, want SERVFAIL", resp.Rcode)
	}
	tg.fresh.Store(true)
	resp = query(t, addr, "web.prod.rune", mdns.TypeA)
	if resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("fresh rcode = %d", resp.Rcode)
	}
}

// TestForward_RoutesNonRune spins up a tiny upstream that always
// answers with a fixed A record and verifies non-.rune queries are
// proxied to it.
func TestForward_RoutesNonRune(t *testing.T) {
	upAddr := freePort(t)
	upMux := mdns.NewServeMux()
	upMux.HandleFunc(".", func(w mdns.ResponseWriter, r *mdns.Msg) {
		resp := new(mdns.Msg)
		resp.SetReply(r)
		resp.Answer = append(resp.Answer, &mdns.A{
			Hdr: mdns.RR_Header{Name: r.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 30},
			A:   net.IPv4(8, 8, 8, 8),
		})
		_ = w.WriteMsg(resp)
	})
	upSrv := &mdns.Server{Addr: upAddr, Net: "udp", Handler: upMux}
	go func() { _ = upSrv.ListenAndServe() }()
	t.Cleanup(func() { _ = upSrv.Shutdown() })
	time.Sleep(50 * time.Millisecond)

	z := &fakeZone{}
	_, addr := startServer(t, Config{
		Zone:             z,
		UpstreamProvider: func() []string { return []string{upAddr} },
	})

	resp := query(t, addr, "example.com", mdns.TypeA)
	if resp.Rcode != mdns.RcodeSuccess || len(resp.Answer) == 0 {
		t.Fatalf("forwarded answer missing: %+v", resp)
	}
	a := resp.Answer[0].(*mdns.A)
	if !a.A.Equal(net.IPv4(8, 8, 8, 8)) {
		t.Errorf("got %s", a.A)
	}
}

func TestForward_NoUpstreamsServfail(t *testing.T) {
	z := &fakeZone{}
	// Empty upstream provider -> refresh returns error and upstreams
	// stay empty. Forwarder should SERVFAIL.
	_, addr := startServer(t, Config{
		Zone:             z,
		UpstreamProvider: func() []string { return nil },
	})
	resp := query(t, addr, "example.com", mdns.TypeA)
	if resp.Rcode != mdns.RcodeServerFailure {
		t.Fatalf("rcode = %d, want SERVFAIL", resp.Rcode)
	}
}

func TestRefresh_PicksUpNewUpstreams(t *testing.T) {
	z := &fakeZone{}
	current := atomic.Pointer[[]string]{}
	empty := []string{}
	current.Store(&empty)

	s, addr := startServer(t, Config{
		Zone:             z,
		UpstreamProvider: func() []string { return *current.Load() },
	})
	resp := query(t, addr, "example.com", mdns.TypeA)
	if resp.Rcode != mdns.RcodeServerFailure {
		t.Fatalf("initial rcode = %d", resp.Rcode)
	}

	// Bring up an upstream and signal refresh.
	upAddr := freePort(t)
	upMux := mdns.NewServeMux()
	upMux.HandleFunc(".", func(w mdns.ResponseWriter, r *mdns.Msg) {
		resp := new(mdns.Msg)
		resp.SetReply(r)
		resp.Answer = append(resp.Answer, &mdns.A{
			Hdr: mdns.RR_Header{Name: r.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 30},
			A:   net.IPv4(1, 1, 1, 1),
		})
		_ = w.WriteMsg(resp)
	})
	upSrv := &mdns.Server{Addr: upAddr, Net: "udp", Handler: upMux}
	go func() { _ = upSrv.ListenAndServe() }()
	t.Cleanup(func() { _ = upSrv.Shutdown() })
	time.Sleep(50 * time.Millisecond)

	v := []string{upAddr}
	current.Store(&v)
	if err := s.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	resp = query(t, addr, "example.com", mdns.TypeA)
	if resp.Rcode != mdns.RcodeSuccess || len(resp.Answer) == 0 {
		t.Fatalf("post-refresh rcode = %d, ans = %+v", resp.Rcode, resp.Answer)
	}
}

func TestSplitName(t *testing.T) {
	cases := []struct {
		in      string
		svc, ns string
		want    bool
	}{
		{"web.prod.rune.", "web", "prod", true},
		{"db.staging.rune.", "db", "staging", true},
		{"rune.", "", "", false},
		{"prod.rune.", "", "", false},  // missing service
		{"a.b.c.rune.", "", "", false}, // too many labels
		{"web.prod.example.com.", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			s, n, ok := splitName(strings.ToLower(c.in))
			if ok != c.want {
				t.Fatalf("ok=%v want %v", ok, c.want)
			}
			if ok && (s != c.svc || n != c.ns) {
				t.Fatalf("got (%s,%s) want (%s,%s)", s, n, c.svc, c.ns)
			}
		})
	}
}
