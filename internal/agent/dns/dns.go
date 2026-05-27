// Package dns implements the per-node embedded DNS server (RUNE-063).
//
// The agent serves an authoritative zone for `.rune` and forwards
// every other query to host upstream resolvers read from
// /etc/resolv.conf. Records are A only and resolve
// `<service>.<namespace>.rune` to the service's stable VIP allocated
// by the control plane (RUNE-040). TTL is intentionally short (5s)
// so dropped/replaced services are noticed quickly.
//
// Bind addresses default to 127.0.0.123:53 (UDP+TCP). 127.0.0.123
// is chosen rather than 127.0.0.53 because the latter is reserved by
// systemd-resolved on most Linux distributions and would conflict.
// Additional bind addresses (typically Docker bridge gateways) can
// be supplied at construction time so containers reach the DNS
// server through their own gateway IP.
//
// The Subsystem implements the same Name / Start / Ready / Stop
// contract as the agent's other subsystems (see internal/agent), but
// does not import the agent package directly to avoid cycles.
package dns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/runestack/rune/pkg/log"
)

// Defaults.
const (
	// DefaultBindAddr is the loopback address the agent binds for
	// host-side DNS injection. Avoids systemd-resolved on .53.
	DefaultBindAddr = "127.0.0.123:53"

	// Zone is the authoritative zone (always trailing dot for miekg).
	Zone = "rune."

	// DefaultTTL is the TTL stamped on every answer.
	DefaultTTL uint32 = 5

	// DefaultStaleBudget mirrors the dataplane: after this much time
	// without a refresh signal, the resolver returns SERVFAIL for
	// .rune queries instead of stale data.
	DefaultStaleBudget = 30 * time.Second

	// resolvConfPath is read at startup and on every Refresh() call.
	resolvConfPath = "/etc/resolv.conf"
)

// ZoneProvider resolves an in-zone name to one or more A records.
// LookupA is called for every <service>.<namespace>.rune query the
// server receives.
type ZoneProvider interface {
	// LookupA returns the IPv4 addresses for service `name` in
	// namespace `ns`. ok=false means the service is not known.
	LookupA(ns, name string) (ips []net.IP, ok bool)
}

// Freshness reports whether the underlying state (orderedlog watch)
// is fresh enough to answer authoritatively. When false, .rune
// queries are answered with SERVFAIL.
type Freshness interface {
	IsFresh() bool
}

// staticFreshness always reports true; useful for tests and dev mode.
type staticFreshness struct{}

func (staticFreshness) IsFresh() bool { return true }

// AlwaysFresh returns a Freshness that always reports fresh.
func AlwaysFresh() Freshness { return staticFreshness{} }

// Config bundles the parameters for constructing a Subsystem.
type Config struct {
	// Zone provides the in-zone resolutions. Required.
	Zone ZoneProvider

	// Freshness gates .rune answers; nil means always fresh.
	Freshness Freshness

	// BindAddrs are host:port pairs to bind. If empty, defaults to
	// just DefaultBindAddr. Each address is bound on both UDP and TCP.
	BindAddrs []string

	// TTL stamped on answers; defaults to DefaultTTL.
	TTL uint32

	// StaleBudget; informational, used by callers to align with the
	// dataplane budget. The Freshness interface is the actual gate.
	StaleBudget time.Duration

	// UpstreamProvider, if set, is consulted for the forwarder
	// upstream list on every query. If nil, /etc/resolv.conf is
	// parsed at Start and on each Refresh() call.
	UpstreamProvider func() []string

	// ForwardTimeout caps each upstream query; defaults to 2s.
	ForwardTimeout time.Duration

	// Logger; defaults to the global logger with component "dns".
	Logger log.Logger
}

// Subsystem is the per-node embedded DNS server.
type Subsystem struct {
	cfg Config
	log log.Logger

	mu        sync.RWMutex
	upstreams []string

	servers  []*dns.Server
	conns    []io.Closer // underlying UDP/TCP conns; closed by Stop
	startMu  sync.Mutex
	started  bool
	stopped  bool
	readyCh  chan struct{}
	closeWG  sync.WaitGroup
	fwClient *dns.Client
}

// New constructs a Subsystem. The server is not bound until Start.
func New(cfg Config) (*Subsystem, error) {
	if cfg.Zone == nil {
		return nil, errors.New("dns: nil ZoneProvider")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger().WithComponent("dns")
	} else {
		cfg.Logger = cfg.Logger.WithComponent("dns")
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.StaleBudget <= 0 {
		cfg.StaleBudget = DefaultStaleBudget
	}
	if cfg.ForwardTimeout <= 0 {
		cfg.ForwardTimeout = 2 * time.Second
	}
	if len(cfg.BindAddrs) == 0 {
		cfg.BindAddrs = []string{DefaultBindAddr}
	}
	if cfg.Freshness == nil {
		cfg.Freshness = AlwaysFresh()
	}
	return &Subsystem{
		cfg:      cfg,
		log:      cfg.Logger,
		readyCh:  make(chan struct{}),
		fwClient: &dns.Client{Timeout: cfg.ForwardTimeout},
	}, nil
}

// Name implements the agent Subsystem contract.
func (s *Subsystem) Name() string { return "dns" }

// Ready returns a channel closed when the server has bound and is
// serving on at least one address.
func (s *Subsystem) Ready() <-chan struct{} { return s.readyCh }

// BindAddrs returns the resolved bind addresses (post-defaulting),
// formatted as `host:port`. Used by the runed startup glue to wire
// DNS injection: once this subsystem is Ready, we tell the docker
// runner to inject these addresses into every subsequently-created
// container so they can resolve `<service>.<namespace>.rune`.
func (s *Subsystem) BindAddrs() []string {
	out := make([]string, len(s.cfg.BindAddrs))
	copy(out, s.cfg.BindAddrs)
	return out
}

// Start binds the configured addresses on UDP+TCP and begins serving.
// It blocks until at least one bind succeeds or all binds fail.
func (s *Subsystem) Start(ctx context.Context) error {
	s.startMu.Lock()
	if s.started {
		s.startMu.Unlock()
		return errors.New("dns: already started")
	}
	s.started = true
	s.startMu.Unlock()

	if err := s.refreshUpstreams(); err != nil {
		s.log.Warn("upstream parse failed at start; forwarder disabled until refresh",
			log.Err(err))
	}

	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handle)

	// Bind every listener synchronously and hand the live conn to the
	// dns.Server via ActivateAndServe. Binding inside the serve
	// goroutine (ListenAndServe) raced Stop: a ShutdownContext that
	// landed before the goroutine marked the server "started" was a
	// silent no-op in miekg/dns, so the listener served forever and
	// Stop's closeWG.Wait() deadlocked. Owning the conn lets Stop
	// terminate the serve loop by closing it directly.
	var ok bool
	for _, addr := range s.cfg.BindAddrs {
		udpConn, err := net.ListenPacket("udp", addr)
		if err != nil {
			s.log.Warn("dns: udp bind failed", log.Str("addr", addr), log.Err(err))
			continue
		}
		tcpLn, err := net.Listen("tcp", addr)
		if err != nil {
			_ = udpConn.Close()
			s.log.Warn("dns: tcp bind failed", log.Str("addr", addr), log.Err(err))
			continue
		}
		s.conns = append(s.conns, udpConn, tcpLn)
		for _, srv := range []*dns.Server{
			{PacketConn: udpConn, Handler: mux},
			{Listener: tcpLn, Handler: mux},
		} {
			srv := srv
			s.servers = append(s.servers, srv)
			s.closeWG.Add(1)
			go func() {
				defer s.closeWG.Done()
				if err := srv.ActivateAndServe(); err != nil {
					s.log.Warn("dns listener stopped", log.Str("addr", addr), log.Err(err))
				}
			}()
		}
		ok = true
	}
	if !ok {
		return errors.New("dns: no bind addresses")
	}
	close(s.readyCh)
	go func() {
		<-ctx.Done()
		_ = s.Stop(context.Background())
	}()
	return nil
}

// Stop shuts down all listeners. Closing each underlying conn always
// unblocks its ActivateAndServe loop — unlike dns.Server.Shutdown,
// which silently no-ops when it races a not-yet-"started" server and
// leaves the listener (and this wait) hung forever. The wait is also
// bounded so a stuck listener can never wedge agent shutdown/rollback.
func (s *Subsystem) Stop(_ context.Context) error {
	s.startMu.Lock()
	if s.stopped {
		s.startMu.Unlock()
		return nil
	}
	s.stopped = true
	conns := s.conns
	s.startMu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}

	done := make(chan struct{})
	go func() { s.closeWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.log.Warn("dns: listeners did not exit within 5s; continuing shutdown")
	}
	return nil
}

// Refresh re-reads /etc/resolv.conf for upstream resolvers. Wire this
// to SIGHUP at the daemon level. Returns an error if no upstreams
// could be loaded; the previous list is retained on error.
func (s *Subsystem) Refresh() error {
	return s.refreshUpstreams()
}

func (s *Subsystem) refreshUpstreams() error {
	var ups []string
	if s.cfg.UpstreamProvider != nil {
		ups = s.cfg.UpstreamProvider()
	} else {
		var err error
		ups, err = parseResolvConf(resolvConfPath, bindIPSet(s.cfg.BindAddrs))
		if err != nil {
			return err
		}
	}
	if len(ups) == 0 {
		return errors.New("dns: no upstream resolvers")
	}
	s.mu.Lock()
	s.upstreams = ups
	s.mu.Unlock()
	return nil
}

func (s *Subsystem) handle(w dns.ResponseWriter, r *dns.Msg) {
	if r == nil || len(r.Question) == 0 {
		s.replyRcode(w, r, dns.RcodeFormatError)
		return
	}
	q := r.Question[0]
	name := strings.ToLower(q.Name)
	if strings.HasSuffix(name, "."+Zone) || name == Zone {
		s.handleZone(w, r, q, name)
		return
	}
	s.handleForward(w, r)
}

// handleZone answers in-zone queries authoritatively.
//
// Format: "<service>.<namespace>.rune." (3 labels + zone + root).
// Anything else in-zone is NXDOMAIN.
func (s *Subsystem) handleZone(w dns.ResponseWriter, r *dns.Msg, q dns.Question, name string) {
	if !s.cfg.Freshness.IsFresh() {
		s.replyRcode(w, r, dns.RcodeServerFailure)
		return
	}

	resp := new(dns.Msg)
	resp.SetReply(r)
	resp.Authoritative = true
	resp.RecursionAvailable = true

	// Only A and AAAA are interesting; AAAA returns NOERROR/empty.
	switch q.Qtype {
	case dns.TypeA:
		// fallthrough below
	case dns.TypeAAAA, dns.TypeANY:
		// We only have A records. Empty NOERROR is the canonical
		// answer to a question we can't satisfy in this zone.
		_ = w.WriteMsg(resp)
		return
	default:
		_ = w.WriteMsg(resp)
		return
	}

	svc, ns, ok := splitName(name)
	if !ok {
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}
	ips, ok := s.cfg.Zone.LookupA(ns, svc)
	if !ok || len(ips) == 0 {
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    s.cfg.TTL,
			},
			A: v4,
		})
	}
	_ = w.WriteMsg(resp)
}

// handleForward proxies the query to the first upstream that
// answers. Returns SERVFAIL if no upstream responds.
func (s *Subsystem) handleForward(w dns.ResponseWriter, r *dns.Msg) {
	s.mu.RLock()
	ups := append([]string(nil), s.upstreams...)
	s.mu.RUnlock()
	if len(ups) == 0 {
		s.replyRcode(w, r, dns.RcodeServerFailure)
		return
	}
	for _, u := range ups {
		ans, _, err := s.fwClient.Exchange(r, u)
		if err == nil && ans != nil {
			_ = w.WriteMsg(ans)
			return
		}
	}
	s.replyRcode(w, r, dns.RcodeServerFailure)
}

func (s *Subsystem) replyRcode(w dns.ResponseWriter, r *dns.Msg, rcode int) {
	if r == nil {
		return
	}
	resp := new(dns.Msg)
	resp.SetReply(r)
	resp.Rcode = rcode
	_ = w.WriteMsg(resp)
}

// splitName parses "<service>.<namespace>.rune." -> (service, ns).
func splitName(name string) (svc, ns string, ok bool) {
	trimmed := strings.TrimSuffix(name, "."+Zone)
	if trimmed == name {
		return "", "", false
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 2 {
		return "", "", false
	}
	if parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// parseResolvConf reads /etc/resolv.conf and returns the configured
// nameservers as host:53 strings. Skips entries whose IP exactly
// matches one of the agent's own bind IPs — forwarding to ourselves
// would loop. Other loopback addresses (notably systemd-resolved's
// 127.0.0.53 stub, which is the *only* nameserver in /etc/resolv.conf
// on stock Ubuntu) are kept: they're distinct services and the agent
// must forward to them or external DNS breaks.
func parseResolvConf(path string, skipIPs map[string]struct{}) ([]string, error) {
	cc, err := dns.ClientConfigFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("dns: parse %s: %w", path, err)
	}
	out := make([]string, 0, len(cc.Servers))
	for _, srv := range cc.Servers {
		if ip := net.ParseIP(srv); ip != nil {
			if _, skip := skipIPs[ip.String()]; skip {
				continue
			}
		}
		out = append(out, net.JoinHostPort(srv, cc.Port))
	}
	return out, nil
}

// bindIPSet extracts the IP portion of each host:port address and
// returns a set, used by parseResolvConf to avoid registering the
// agent itself as an upstream. Bridge-gateway binds added dynamically
// aren't included, but they also never appear in /etc/resolv.conf —
// the filter is for the loopback bind only.
func bindIPSet(addrs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(addrs))
	for _, addr := range addrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if ip := net.ParseIP(host); ip != nil {
			out[ip.String()] = struct{}{}
		}
	}
	return out
}
