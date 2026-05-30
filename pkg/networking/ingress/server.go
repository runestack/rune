package ingress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
)

// Config bundles the dependencies the ingress Subsystem needs.
type Config struct {
	// Router is the live host -> service table. Required.
	Router *Router

	// Challenges serves HTTP-01 ACME challenge tokens. Required.
	// Reads come on the HTTP listener; writes are made by the
	// ACME orchestrator.
	Challenges *MemChallengeStore

	// Certs is the TLS certificate loader. Required.
	Certs *CertLoader

	// ClientCAs holds per-host client-CA pools for inbound mTLS
	// (origin hardening). Optional; nil disables client-cert
	// verification entirely. When set, hosts with a registered pool
	// require + verify a client cert at the handshake.
	ClientCAs *ClientCARegistry

	// HTTPAddr is the bind address for the plain HTTP listener.
	// Default ":80". Use ":8080" or similar in dev mode to avoid
	// requiring CAP_NET_BIND_SERVICE.
	HTTPAddr string

	// HTTPSAddr is the bind address for the HTTPS listener.
	// Default ":443". Use ":8443" in dev mode. Empty disables TLS.
	HTTPSAddr string

	// UpstreamResolver maps (namespace, service, port) to a dial
	// target ("ip:port"). Returning an empty string means the
	// service is not currently reachable; the listener will return
	// 503. The data plane normally satisfies this via the local
	// VIP (RUNE-041); a stub returning vip:port is fine.
	UpstreamResolver UpstreamResolver

	// Logger is the structured logger. Defaults to "ingress".
	Logger log.Logger

	// ReadyTimeout caps how long Start blocks waiting for the
	// listeners to bind. Defaults to 5s.
	ReadyTimeout time.Duration
}

// UpstreamResolver returns the dial target for a service+port.
type UpstreamResolver interface {
	Resolve(namespace, service string, port int) (target string, ok bool)
}

// FuncResolver lets callers pass a closure where an UpstreamResolver
// is required.
type FuncResolver func(namespace, service string, port int) (string, bool)

// Resolve calls f.
func (f FuncResolver) Resolve(ns, svc string, port int) (string, bool) { return f(ns, svc, port) }

func (c *Config) defaults() {
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":80"
	}
	if c.Logger == nil {
		c.Logger = log.GetDefaultLogger().WithComponent("ingress")
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = 5 * time.Second
	}
}

// Subsystem implements the agent.Subsystem contract for the
// ingress controller. Lifecycle:
//
//   - Start binds both listeners and reports ready once both bind
//     successfully (or, if HTTPSAddr is empty, only HTTP).
//   - Stop closes the listeners and waits for in-flight handlers
//     to drain (capped at 30s).
type Subsystem struct {
	cfg Config

	mu       sync.Mutex
	httpSrv  *http.Server
	httpsSrv *http.Server
	httpLn   net.Listener
	httpsLn  net.Listener
	started  bool
	stopped  bool
	readyCh  chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Name returns the subsystem identifier used in logs and metrics.
func (s *Subsystem) Name() string { return "ingress" }

// Ready reports when both listeners are bound and serving.
func (s *Subsystem) Ready() <-chan struct{} { return s.readyCh }

// New constructs a Subsystem. Required Config fields are validated
// up front so misconfiguration fails fast at runed startup.
func New(cfg Config) (*Subsystem, error) {
	if cfg.Router == nil {
		return nil, errors.New("ingress: Router is required")
	}
	if cfg.Challenges == nil {
		return nil, errors.New("ingress: Challenges is required")
	}
	if cfg.Certs == nil {
		return nil, errors.New("ingress: Certs is required")
	}
	if cfg.UpstreamResolver == nil {
		return nil, errors.New("ingress: UpstreamResolver is required")
	}
	cfg.defaults()
	return &Subsystem{
		cfg:     cfg,
		readyCh: make(chan struct{}),
	}, nil
}

// Start binds both listeners and runs them in background goroutines.
// Returns the first bind error.
func (s *Subsystem) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("ingress: already started")
	}
	s.started = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()

	// HTTP listener.
	httpLn, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		cancel()
		return fmt.Errorf("ingress: bind http: %w", err)
	}
	s.httpLn = httpLn
	s.httpSrv = &http.Server{
		Handler:           http.HandlerFunc(s.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.cfg.Logger.Warn("ingress http listener exited", log.Err(err))
		}
	}()
	s.cfg.Logger.Info("ingress http listening", log.Str("addr", httpLn.Addr().String()))

	// HTTPS listener (optional).
	if s.cfg.HTTPSAddr != "" {
		httpsLn, err := net.Listen("tcp", s.cfg.HTTPSAddr)
		if err != nil {
			_ = httpLn.Close()
			cancel()
			return fmt.Errorf("ingress: bind https: %w", err)
		}
		s.httpsLn = httpsLn
		tlsCfg := &tls.Config{
			GetCertificate: s.cfg.Certs.GetCertificate,
			MinVersion:     tls.VersionTLS12,
			NextProtos:     []string{"h2", "http/1.1"},
		}
		// Per-SNI inbound mTLS (origin hardening). Client-cert
		// verification is negotiated during the handshake, before the
		// HTTP Host is known, so we vary the config by SNI: hosts with a
		// registered client-CA pool require + verify a client cert; all
		// others fall through to the base config (no client auth).
		if s.cfg.ClientCAs != nil {
			base := tlsCfg.Clone()
			tlsCfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				return s.cfg.ClientCAs.ConfigFor(hello, base), nil
			}
		}
		s.httpsSrv = &http.Server{
			Handler:           http.HandlerFunc(s.serveHTTPS),
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 10 * time.Second,
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			tlsLn := tls.NewListener(httpsLn, tlsCfg)
			if err := s.httpsSrv.Serve(tlsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.cfg.Logger.Warn("ingress https listener exited", log.Err(err))
			}
		}()
		s.cfg.Logger.Info("ingress https listening", log.Str("addr", httpsLn.Addr().String()))
	}

	// Use the supplied ctx for cancellation propagation: when ctx
	// fires, trigger the runCtx so future shutdown sequencing is
	// coupled to caller intent.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()

	close(s.readyCh)
	return nil
}

// Stop closes the listeners and waits for handlers to drain. Always
// honors a 30s drain cap regardless of the supplied ctx so a
// panicked handler does not wedge runed shutdown forever.
func (s *Subsystem) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer drainCancel()
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(drainCtx)
	}
	if s.httpsSrv != nil {
		_ = s.httpsSrv.Shutdown(drainCtx)
	}
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		s.cfg.Logger.Warn("ingress shutdown drain timeout")
	}
	_ = ctx
	return nil
}

// serveHTTP handles port-80 requests. Two cases:
//   - /.well-known/acme-challenge/<token> — serve from challenge store.
//   - everything else — proxy to the upstream resolved from Host.
func (s *Subsystem) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
		token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		if v, ok := s.cfg.Challenges.Get(token); ok {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, v)
			return
		}
		http.NotFound(w, r)
		return
	}
	s.proxy(w, r, "http")
}

// serveHTTPS handles port-443 requests. Always proxy.
func (s *Subsystem) serveHTTPS(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, "https")
}

// peerIP extracts the IP from an http.Request.RemoteAddr ("ip:port", set
// by the server from the connection — not spoofable via headers). Returns
// nil when it can't be parsed, which PeerAllowed treats as deny under an
// active allowlist.
func peerIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // already a bare IP?
	}
	return net.ParseIP(host)
}

// mtlsGate decides whether a request may proceed for a route that requires
// inbound mTLS (expose.clientCert). It fails CLOSED: plaintext (:80) is
// never allowed for an mTLS-locked origin (the handshake — and thus client
// cert verification — only happens on :443), and even on :443 the per-host
// CA pool must be loaded, else GetConfigForClient would fall through to the
// no-client-auth base config and serve the origin unauthenticated. Returns
// (status, false) to reject, or (0, true) to proceed.
func mtlsGate(rt Route, scheme string, hasPool bool) (int, bool) {
	if !rt.RequireClientCert {
		return 0, true
	}
	if scheme != "https" {
		// Plaintext can never carry a verified client cert.
		return http.StatusForbidden, false
	}
	if !hasPool {
		// Required but the CA isn't loaded (e.g. bad/missing caSecret).
		// Refuse rather than serve without verifying the client.
		return http.StatusServiceUnavailable, false
	}
	return 0, true
}

func (s *Subsystem) proxy(w http.ResponseWriter, r *http.Request, scheme string) {
	rt, ok := s.cfg.Router.Match(r.Host, r.URL.Path)
	if !ok {
		http.Error(w, "no route for host "+r.Host, http.StatusNotFound)
		return
	}
	// Source-IP allowlist (origin hardening). Checked against the real TCP
	// peer (r.RemoteAddr), never a forwarding header which the client can
	// set. Empty allowlist = allow all. See RUNE-0XX.
	if !rt.PeerAllowed(peerIP(r.RemoteAddr)) {
		s.cfg.Logger.Warn("ingress: source not in allowCidrs; rejecting",
			log.Str("host", r.Host),
			log.Str("peer", r.RemoteAddr))
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Inbound mTLS (origin hardening). Fail closed: refuse plaintext for an
	// mTLS-locked host, and refuse :443 when the CA pool isn't loaded — so a
	// clientCert origin is never served unauthenticated. See RUNE-0XX.
	hasPool := s.cfg.ClientCAs.HasPool(rt.Host)
	if code, allow := mtlsGate(rt, scheme, hasPool); !allow {
		s.cfg.Logger.Warn("ingress: clientCert required; rejecting",
			log.Str("host", r.Host),
			log.Str("scheme", scheme),
			log.Bool("caLoaded", hasPool),
			log.Int("status", code))
		http.Error(w, "forbidden", code)
		return
	}
	target, ok := s.cfg.UpstreamResolver.Resolve(rt.Namespace, rt.Service, rt.Port)
	if !ok || target == "" {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	upstream := &url.URL{
		Scheme: "http",
		Host:   target,
	}
	rp := httputil.NewSingleHostReverseProxy(upstream)
	rp.ErrorLog = nil
	rp.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		s.cfg.Logger.Warn("ingress proxy error",
			log.Str("host", req.Host),
			log.Str("path", req.URL.Path),
			log.Err(err))
		http.Error(rw, "bad gateway: "+err.Error(), http.StatusBadGateway)
	}
	// Preserve the original Host header.
	origHost := r.Host
	director := rp.Director
	rp.Director = func(req *http.Request) {
		director(req)
		req.Host = origHost
		req.Header.Set("X-Forwarded-Host", origHost)
		req.Header.Set("X-Forwarded-Proto", scheme)
	}
	rp.ServeHTTP(w, r)
}

// HTTPAddr returns the bound HTTP listener address. Useful for
// tests using port :0.
func (s *Subsystem) HTTPAddr() string {
	if s.httpLn == nil {
		return ""
	}
	return s.httpLn.Addr().String()
}

// HTTPSAddr returns the bound HTTPS listener address.
func (s *Subsystem) HTTPSAddr() string {
	if s.httpsLn == nil {
		return ""
	}
	return s.httpsLn.Addr().String()
}

// JoinHostPort is a tiny convenience for callers building dial
// targets without importing net.
func JoinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
