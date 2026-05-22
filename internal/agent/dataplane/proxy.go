package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/policy"
	"github.com/runestack/rune/pkg/types"
)

// freshFn is the dataplane's "is the watch stream within the staleness
// budget?" predicate. The proxy refuses new connections when fresh()
// returns false (fail-closed behavior on prolonged watch disconnect).
type freshFn func() bool

// policyEvalFn evaluates ingress policy for a connection landing on
// serviceID from srcIP on (port, proto). Nil means "no policy
// enforcement wired in" (treated as allow). The Subsystem owns the
// real implementation and resolves srcIP -> identity through the
// LocalInstances table internally.
type policyEvalFn func(serviceID string, srcIP net.IP, port int, proto string) policy.Result

// listenerKey identifies a single VIP+port+protocol listener.
type listenerKey struct {
	serviceID string
	port      int
	protocol  string // "tcp" or "udp"
}

// ProxyManager owns the set of per-VIP listeners and supervises their
// lifecycle. It is the dataplane's only network-facing component.
type ProxyManager struct {
	cfg     Config
	cache   *Cache
	metrics *Metrics
	fresh   freshFn
	eval    policyEvalFn

	mu        sync.Mutex
	stopped   bool
	listeners map[listenerKey]*listener
	services  map[string]*types.Service // serviceID -> last-known spec
}

func newProxyManager(cfg Config, cache *Cache, m *Metrics, fresh freshFn, eval policyEvalFn) *ProxyManager {
	return &ProxyManager{
		cfg:       cfg,
		cache:     cache,
		metrics:   m,
		fresh:     fresh,
		eval:      eval,
		listeners: make(map[listenerKey]*listener),
		services:  make(map[string]*types.Service),
	}
}

// PortReserved reports whether port is in the dataplane's reserved set
// (ingress-owned ports on an edge node, typically 80/443). Exported so
// the ingress controller shares one definition of "reserved port"
// rather than carrying its own copy.
func PortReserved(reserved []int, port int) bool {
	for _, r := range reserved {
		if r == port {
			return true
		}
	}
	return false
}

// Register reconciles listeners for svc. Adds new listeners for new
// ports, removes listeners for removed ports, leaves unchanged ones
// alone. Returns the first failure if any port couldn't bind.
func (pm *ProxyManager) Register(svc *types.Service) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.stopped {
		return errors.New("dataplane: proxy stopped")
	}
	if svc.ID == "" {
		return errors.New("dataplane: service has no ID")
	}

	bindIP, err := pm.bindIPFor(svc)
	if err != nil {
		return err
	}
	pm.services[svc.ID] = svc

	wantKeys := map[listenerKey]struct{}{}
	for _, p := range svc.Ports {
		// Skip ports the ingress owns on an edge node — a <vip>:80
		// listener collides with the ingress 0.0.0.0:80 wildcard bind.
		if PortReserved(pm.cfg.ReservedHostPorts, p.Port) {
			pm.cfg.Logger.Debug("dataplane: skipping VIP listener on ingress-reserved port",
				log.Str("service", svc.Name), log.Int("port", p.Port))
			continue
		}
		proto := normalizeProtocol(p.Protocol)
		key := listenerKey{serviceID: svc.ID, port: p.Port, protocol: proto}
		wantKeys[key] = struct{}{}
		if _, exists := pm.listeners[key]; exists {
			continue
		}
		ln, err := pm.openListener(svc, p, proto, bindIP)
		if err != nil {
			return fmt.Errorf("dataplane: listen %s/%d (%s): %w", proto, p.Port, bindIP, err)
		}
		pm.listeners[key] = ln
		pm.metrics.observeListenerOpened(svc.ID, proto)
	}

	// Tear down any listener for this service that no longer
	// corresponds to a port in the spec.
	for key, ln := range pm.listeners {
		if key.serviceID != svc.ID {
			continue
		}
		if _, ok := wantKeys[key]; ok {
			continue
		}
		ln.shutdown(pm.cfg.DrainTimeout)
		delete(pm.listeners, key)
		pm.metrics.observeListenerClosed(svc.ID, key.protocol)
	}
	return nil
}

// Unregister tears down all listeners for serviceID.
func (pm *ProxyManager) Unregister(serviceID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for key, ln := range pm.listeners {
		if key.serviceID != serviceID {
			continue
		}
		ln.shutdown(pm.cfg.DrainTimeout)
		delete(pm.listeners, key)
		pm.metrics.observeListenerClosed(serviceID, key.protocol)
	}
	delete(pm.services, serviceID)
}

// Shutdown closes all listeners, draining for at most ctx's deadline.
func (pm *ProxyManager) Shutdown(ctx context.Context) {
	pm.mu.Lock()
	pm.stopped = true
	all := pm.listeners
	pm.listeners = map[listenerKey]*listener{}
	pm.mu.Unlock()

	deadline := pm.cfg.DrainTimeout
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining < deadline {
			deadline = remaining
		}
	}
	var wg sync.WaitGroup
	for _, ln := range all {
		ln := ln
		wg.Add(1)
		go func() { defer wg.Done(); ln.shutdown(deadline) }()
	}
	wg.Wait()
}

// Snapshot returns one entry per active listener for diagnostics.
func (pm *ProxyManager) Snapshot() []ListenerSummary {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make([]ListenerSummary, 0, len(pm.listeners))
	for k, ln := range pm.listeners {
		out = append(out, ListenerSummary{
			ServiceID:  k.serviceID,
			Port:       k.port,
			TargetPort: ln.targetPort,
			Protocol:   k.protocol,
			Addr:       ln.addr,
			Active:     ln.activeConns(),
		})
	}
	return out
}

// ListenerSummary is the diagnostic view of a single listener.
type ListenerSummary struct {
	ServiceID string
	Port      int
	// TargetPort is the upstream container port this listener dials.
	// Defaults to Port when the service spec leaves TargetPort unset.
	TargetPort int
	Protocol   string
	Addr       string
	Active     int64
}

// bindIPFor returns the local interface IP the listener should bind.
// In dev mode that's 127.0.0.1; in production it's the service's VIP.
func (pm *ProxyManager) bindIPFor(svc *types.Service) (net.IP, error) {
	if pm.cfg.Mode == ModeDev {
		return net.ParseIP("127.0.0.1"), nil
	}
	if svc.Discovery == nil || svc.Discovery.VIP == "" {
		return nil, fmt.Errorf("dataplane: service %s has no VIP (allocator not run?)", svc.ID)
	}
	ip := net.ParseIP(svc.Discovery.VIP)
	if ip == nil {
		return nil, fmt.Errorf("dataplane: service %s has invalid VIP %q", svc.ID, svc.Discovery.VIP)
	}
	return ip, nil
}

// openListener binds the socket and starts the accept goroutine.
func (pm *ProxyManager) openListener(svc *types.Service, p types.ServicePort, proto string, bindIP net.IP) (*listener, error) {
	addr := net.JoinHostPort(bindIP.String(), strconv.Itoa(p.Port))
	pref := ""
	if svc.Discovery != nil {
		pref = svc.Discovery.LocalityPreference
	}
	localNode := ""
	if pm.cfg.Node != nil {
		localNode = pm.cfg.Node.NodeID()
	}

	// An unset TargetPort means "same as the service port" (Kubernetes
	// semantics). Without this default a multi-port service whose
	// ports omit targetPort left every listener with targetPort 0, so
	// endpointPort() fell through to the endpoint's single advertised
	// port (the primary) — e.g. flo's 9001/9002 VIP listeners all
	// dialled the container's 9000.
	targetPort := p.TargetPort
	if targetPort == 0 {
		targetPort = p.Port
	}

	l := &listener{
		key:         listenerKey{serviceID: svc.ID, port: p.Port, protocol: proto},
		serviceID:   svc.ID,
		namespace:   svc.Namespace,
		servicePort: p.Port,
		targetPort:  targetPort,
		addr:        addr,
		log:         pm.cfg.Logger.WithComponent("proxy"),
		cache:       pm.cache,
		metrics:     pm.metrics,
		fresh:       pm.fresh,
		eval:        pm.eval,
		pref:        pref,
		localNode:   localNode,
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // G404: non-cryptographic RNG for endpoint load-balancing
		drain:       pm.cfg.DrainTimeout,
		stop:        make(chan struct{}),
	}
	if proto == "udp" {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return nil, err
		}
		l.udp = pc
		go l.serveUDP()
	} else {
		nl, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		l.tcp = nl
		go l.serveTCP()
	}
	pm.cfg.Logger.Info("proxy listener opened",
		log.Str("service_id", svc.ID),
		log.Str("addr", addr),
		log.Str("protocol", proto),
	)
	return l, nil
}

func normalizeProtocol(p string) string {
	switch strings.ToLower(p) {
	case "udp":
		return "udp"
	default:
		return "tcp"
	}
}

// remoteIP extracts the source IP from a net.Addr (TCP or UDP).
// Returns nil for unsupported address types.
func remoteIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.TCPAddr:
		return v.IP
	case *net.UDPAddr:
		return v.IP
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// listener is a single VIP+port+protocol bind. The accept loop runs
// in its own goroutine; closing stop and the underlying socket causes
// the loop to exit.
type listener struct {
	key         listenerKey
	serviceID   string
	namespace   string
	servicePort int
	targetPort  int
	addr        string
	log         log.Logger
	cache       *Cache
	metrics     *Metrics
	fresh       freshFn
	eval        policyEvalFn
	pref        string
	localNode   string
	rng         *rand.Rand
	drain       time.Duration

	tcp net.Listener
	udp net.PacketConn

	stopOnce sync.Once
	stop     chan struct{}

	active int64 // active connections (atomic)
	wg     sync.WaitGroup
}

func (l *listener) activeConns() int64 { return atomic.LoadInt64(&l.active) }

// shutdown closes the listener and waits up to drain for in-flight
// goroutines to finish.
func (l *listener) shutdown(drain time.Duration) {
	l.stopOnce.Do(func() {
		close(l.stop)
		if l.tcp != nil {
			_ = l.tcp.Close()
		}
		if l.udp != nil {
			_ = l.udp.Close()
		}
	})
	if drain <= 0 {
		drain = 100 * time.Millisecond
	}
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(drain):
		l.log.Warn("listener drain timeout, forcing close",
			log.Str("addr", l.addr),
			log.F("active", atomic.LoadInt64(&l.active)),
		)
	}
}

// serveTCP is the per-listener accept loop.
func (l *listener) serveTCP() {
	for {
		conn, err := l.tcp.Accept()
		if err != nil {
			select {
			case <-l.stop:
				return
			default:
			}
			// Transient accept errors (rare): log and continue.
			if !errors.Is(err, net.ErrClosed) {
				l.log.Warn("tcp accept failed", log.Err(err))
			}
			return
		}
		l.wg.Add(1)
		atomic.AddInt64(&l.active, 1)
		l.metrics.incActive(l.serviceID, "tcp")
		go func(c net.Conn) {
			defer l.wg.Done()
			defer atomic.AddInt64(&l.active, -1)
			defer l.metrics.decActive(l.serviceID, "tcp")
			l.handleTCP(c)
		}(conn)
	}
}

// handleTCP picks an endpoint, dials it, and proxies bytes both ways.
func (l *listener) handleTCP(client net.Conn) {
	defer client.Close()

	if !l.fresh() {
		l.metrics.incTotal(l.serviceID, "tcp", "stale_watch")
		l.log.Warn("rejecting connection: watch stream stale",
			log.Str("service_id", l.serviceID),
		)
		return
	}
	if l.eval != nil {
		src := remoteIP(client.RemoteAddr())
		res := l.eval(l.serviceID, src, l.servicePort, "tcp")
		if res.Decision == policy.DecisionDeny {
			l.metrics.incTotal(l.serviceID, "tcp", "policy_denied")
			l.metrics.incPolicyDrop(l.serviceID, l.namespace, l.serviceID, string(res.Reason))
			l.log.Warn("connection dropped by network policy",
				log.Str("service_id", l.serviceID),
				log.Str("namespace", l.namespace),
				log.Str("src", src.String()),
				log.Int("port", l.servicePort),
				log.Str("protocol", "tcp"),
				log.Str("reason", string(res.Reason)),
			)
			return
		}
		if res.Decision == policy.DecisionAllow {
			l.metrics.incPolicyAllow(l.serviceID, l.namespace, l.serviceID)
		}
	}
	healthy, ok := l.cache.Healthy(l.serviceID)
	if !ok || len(healthy) == 0 {
		l.metrics.incTotal(l.serviceID, "tcp", "no_endpoints")
		return
	}
	ep, picked := selectEndpoint(healthy, l.pref, l.localNode, l.rng)
	if !picked {
		l.metrics.incTotal(l.serviceID, "tcp", "no_local_endpoint")
		return
	}

	target := net.JoinHostPort(ep.IP, strconv.Itoa(l.endpointPort(ep)))
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	upstream, err := dialer.Dial("tcp", target)
	if err != nil {
		l.metrics.incTotal(l.serviceID, "tcp", "dial_failed")
		l.log.Warn("upstream dial failed",
			log.Str("target", target),
			log.Str("service_id", l.serviceID),
			log.Err(err),
		)
		return
	}
	defer upstream.Close()

	l.metrics.incTotal(l.serviceID, "tcp", "ok")
	pipe(client, upstream)
}

// endpointPort returns the port to dial on the upstream. We prefer
// the listener's configured TargetPort (Service spec) when present
// because the endpoint set may carry the service-port not the
// container-port for legacy callers; otherwise fall back to the
// endpoint's own Port.
func (l *listener) endpointPort(ep types.Endpoint) int {
	if l.targetPort > 0 {
		return l.targetPort
	}
	return ep.Port
}

// pipe shuttles bytes both directions until either side closes. Each
// half-close is propagated to the other side.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		_ = closeWrite(a)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		_ = closeWrite(b)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// closeWrite half-closes a TCP conn's write side; for non-TCP conns
// it falls back to a full close.
func closeWrite(c net.Conn) error {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// serveUDP is a single goroutine reading datagrams and forwarding
// each to a freshly-selected endpoint. We don't maintain per-flow
// state (no source-IP affinity) — the spec deliberately starts
// stateless.
func (l *listener) serveUDP() {
	buf := make([]byte, 64*1024)
	for {
		n, src, err := l.udp.ReadFrom(buf)
		if err != nil {
			select {
			case <-l.stop:
				return
			default:
			}
			if !errors.Is(err, net.ErrClosed) {
				l.log.Warn("udp read failed", log.Err(err))
			}
			return
		}
		l.metrics.incActive(l.serviceID, "udp")
		atomic.AddInt64(&l.active, 1)
		l.wg.Add(1)
		// Copy buffer because we'll dispatch and reuse buf.
		pkt := append([]byte(nil), buf[:n]...)
		go func(payload []byte, from net.Addr) {
			defer l.wg.Done()
			defer atomic.AddInt64(&l.active, -1)
			defer l.metrics.decActive(l.serviceID, "udp")
			l.handleUDP(payload, from)
		}(pkt, src)
	}
}

func (l *listener) handleUDP(payload []byte, from net.Addr) {
	if !l.fresh() {
		l.metrics.incTotal(l.serviceID, "udp", "stale_watch")
		return
	}
	if l.eval != nil {
		src := remoteIP(from)
		res := l.eval(l.serviceID, src, l.servicePort, "udp")
		if res.Decision == policy.DecisionDeny {
			l.metrics.incTotal(l.serviceID, "udp", "policy_denied")
			l.metrics.incPolicyDrop(l.serviceID, l.namespace, l.serviceID, string(res.Reason))
			l.log.Warn("datagram dropped by network policy",
				log.Str("service_id", l.serviceID),
				log.Str("namespace", l.namespace),
				log.Str("src", src.String()),
				log.Int("port", l.servicePort),
				log.Str("protocol", "udp"),
				log.Str("reason", string(res.Reason)),
			)
			return
		}
		if res.Decision == policy.DecisionAllow {
			l.metrics.incPolicyAllow(l.serviceID, l.namespace, l.serviceID)
		}
	}
	healthy, ok := l.cache.Healthy(l.serviceID)
	if !ok || len(healthy) == 0 {
		l.metrics.incTotal(l.serviceID, "udp", "no_endpoints")
		return
	}
	ep, picked := selectEndpoint(healthy, l.pref, l.localNode, l.rng)
	if !picked {
		l.metrics.incTotal(l.serviceID, "udp", "no_local_endpoint")
		return
	}
	target := net.JoinHostPort(ep.IP, strconv.Itoa(l.endpointPort(ep)))
	upstream, err := net.Dial("udp", target)
	if err != nil {
		l.metrics.incTotal(l.serviceID, "udp", "dial_failed")
		return
	}
	defer upstream.Close()
	if _, err := upstream.Write(payload); err != nil {
		l.metrics.incTotal(l.serviceID, "udp", "write_failed")
		return
	}
	// Best-effort response read with a short deadline.
	_ = upstream.SetReadDeadline(time.Now().Add(2 * time.Second))
	respBuf := make([]byte, 64*1024)
	n, err := upstream.Read(respBuf)
	if err != nil {
		l.metrics.incTotal(l.serviceID, "udp", "ok_no_reply")
		return
	}
	if _, werr := l.udp.WriteTo(respBuf[:n], from); werr != nil {
		l.metrics.incTotal(l.serviceID, "udp", "reply_failed")
		return
	}
	l.metrics.incTotal(l.serviceID, "udp", "ok")
}
