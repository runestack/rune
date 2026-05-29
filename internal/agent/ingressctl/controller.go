// Package ingressctl reconciles the ingress route table from the
// service store and resolves upstream targets from the dataplane
// endpoint cache.
//
// This is the consumer side of the RUNE-066 wiring whose producer
// half lives in pkg/orchestrator/controllers/instance_controller.go
// (PublishService -> OrderedLog -> dataplane.Cache). Without this
// controller, the ingress Subsystem's Router stays empty and every
// inbound request to an `expose.host` returns 404.
package ingressctl

import (
	"context"
	"crypto/x509"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/runestack/rune/internal/agent/dataplane"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/acme"
	"github.com/runestack/rune/pkg/networking/ingress"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// Config bundles the controller's required dependencies.
type Config struct {
	// Router is the ingress route table to keep in sync. Required.
	Router *ingress.Router
	// Store is the agent's local state store. Required.
	Store store.Store
	// Cache is the dataplane endpoint cache used by Resolve to
	// answer ingress.UpstreamResolver lookups. Required.
	Cache *dataplane.Cache
	// ACME is the optional certificate orchestrator. When non-nil,
	// the controller submits a Request for every service whose
	// Expose.TLS asks for ACME on each reconcile.
	ACME *acme.Orchestrator
	// Secrets, when set, lets the controller resolve `tls.mode:
	// manual` exposes by reading the named Secret and pushing its
	// `tls.crt` / `tls.key` data into Certs on each reconcile (so
	// secret rotation flows through without operator action).
	Secrets *repos.SecretRepo
	// Certs is the same CertStore the ACME orchestrator + ingress
	// cert loader are wired against. Required to enable manual
	// mode; nil disables manual-mode handling.
	Certs acme.CertStore
	// ClientCAs is the per-host client-CA registry shared with the
	// ingress listener for inbound mTLS. When set (and Secrets is
	// available), the controller resolves each exposed service's
	// clientCert.caSecret into a pool and pushes it here. nil disables
	// clientCert handling.
	ClientCAs *ingress.ClientCARegistry
	// Logger is the structured logger. Defaults to "ingressctl".
	Logger log.Logger
	// ReconcilePeriod is how often the controller rebuilds the
	// route table from the store as a safety net. Defaults to 2s.
	ReconcilePeriod time.Duration
	// ReservedHostPorts are host ports owned by the edge ingress
	// listener (typically 80/443). The dataplane does not open VIP
	// listeners on these ports, so Resolve must dial container
	// endpoints from the cache instead of the service VIP.
	ReservedHostPorts []int
}

// Controller reconciles ingress routes from the service store and
// implements ingress.UpstreamResolver against the dataplane cache.
type Controller struct {
	cfg Config

	mu        sync.RWMutex
	lastHosts map[string]struct{} // for log noise control

	// svcSnapshot is the service set from the most recent reconcile,
	// keyed by "<namespace>/<name>". Resolve runs on every inbound
	// ingress request, so it reads this in-memory snapshot rather than
	// hitting the store per request. Rebuilt every ReconcilePeriod.
	svcSnapshot map[string]*types.Service

	// manualPushed tracks the (host, secret-version) pair last pushed
	// to the cert store via manual-mode handling. Lets the per-tick
	// reconcile re-push when the underlying secret changes (so secret
	// rotation flows through automatically) but skip the work when
	// nothing changed (so we're not re-parsing a PEM every 2s).
	manualPushed map[string]int

	// clientCAPushed mirrors manualPushed for client-CA pools: the
	// (host, caSecret-version) last pushed to the ClientCAs registry,
	// so a CA rotation re-pushes but steady state skips the PEM parse.
	clientCAPushed map[string]int

	// clientCAHosts is the set of hosts that had a clientCert in the
	// last reconcile, so hosts that drop it get Forget'd from the
	// registry (disabling mTLS) on the next pass.
	clientCAHosts map[string]struct{}

	// manualLogged dedups warning logs on manual-TLS failures so a
	// single bad secret doesn't spam the journal at the 2 s reconcile
	// cadence. Keyed by "<host>::<errKind>"; first failure logs, then
	// suppressed until ManualLogRepeat has elapsed (or the next
	// successful push clears the entry).
	manualLogged map[string]time.Time
}

// ManualLogRepeat is how often we re-emit "still failing" warnings
// for a misconfigured manual-TLS Secret. First failure logs
// immediately; subsequent failures of the same (host, errKind) are
// suppressed until this interval elapses. A successful push clears
// the dedup entry so the next failure logs immediately again.
const ManualLogRepeat = 5 * time.Minute

// New constructs a Controller. Logger and ReconcilePeriod default.
func New(cfg Config) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger().WithComponent("ingressctl")
	}
	if cfg.ReconcilePeriod <= 0 {
		cfg.ReconcilePeriod = 2 * time.Second
	}
	return &Controller{
		cfg:            cfg,
		lastHosts:      map[string]struct{}{},
		manualPushed:   map[string]int{},
		manualLogged:   map[string]time.Time{},
		svcSnapshot:    map[string]*types.Service{},
		clientCAPushed: map[string]int{},
		clientCAHosts:  map[string]struct{}{},
	}
}

// Resolve implements ingress.UpstreamResolver. It prefers the service
// cluster VIP (dataplane proxy) so ingress does not dial stale
// container IPs after restarts. The dataplane cache is keyed by
// service ID; when no VIP is assigned yet, it falls back to the first
// healthy cached endpoint for that ID.
func (c *Controller) Resolve(namespace, service string, port int) (string, bool) {
	if service == "" || port == 0 {
		return "", false
	}
	svc, ok := c.lookupService(namespace, service)
	if !ok {
		return "", false
	}
	// Prefer the cluster VIP: the dataplane proxy owns it and tracks
	// healthy endpoints, so ingress never dials a stale container IP.
	// We return vip:port without confirming the dataplane listener is
	// already open — at worst there's a brief connection-refused
	// window right after startup until the dataplane binds it, which
	// is self-correcting on the client retry.
	if vip := serviceVIP(svc); vip != "" && !dataplane.PortReserved(c.cfg.ReservedHostPorts, port) {
		return net.JoinHostPort(vip, strconv.Itoa(port)), true
	}
	if c.cfg.Cache == nil || svc.ID == "" {
		return "", false
	}
	eps, ok := c.cfg.Cache.Healthy(svc.ID)
	if !ok || len(eps) == 0 {
		return "", false
	}
	ep := eps[0]
	dialPort := ep.Port
	if dialPort == 0 {
		dialPort = port
	}
	if ep.IP == "" || dialPort == 0 {
		return "", false
	}
	return net.JoinHostPort(ep.IP, strconv.Itoa(dialPort)), true
}

// lookupService resolves a service by namespace+name. It serves the
// in-memory snapshot built by the reconcile loop so Resolve stays off
// the store on the request hot path; before the first reconcile (or
// for a service not yet snapshotted) it falls back to a single
// time-bounded store Get.
func (c *Controller) lookupService(namespace, name string) (*types.Service, bool) {
	if name == "" {
		return nil, false
	}
	c.mu.RLock()
	svc, ok := c.svcSnapshot[namespace+"/"+name]
	c.mu.RUnlock()
	if ok {
		return svc, true
	}
	if c.cfg.Store == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var fresh types.Service
	if err := c.cfg.Store.Get(ctx, types.ResourceTypeService, namespace, name, &fresh); err != nil {
		return nil, false
	}
	return &fresh, true
}

func serviceVIP(svc *types.Service) string {
	if svc == nil || svc.Discovery == nil {
		return ""
	}
	return svc.Discovery.VIP
}

// Run blocks until ctx is done, periodically rebuilding the route
// table from the service store. Safe to call once per Controller.
func (c *Controller) Run(ctx context.Context) {
	c.reconcile(ctx)
	t := time.NewTicker(c.cfg.ReconcilePeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.reconcile(ctx)
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) {
	var svcs []types.Service
	if err := c.cfg.Store.ListAll(ctx, types.ResourceTypeService, &svcs); err != nil {
		c.cfg.Logger.Warn("reconcile: list services failed", log.Err(err))
		return
	}
	// Refresh the service snapshot Resolve reads on the request path.
	snapshot := make(map[string]*types.Service, len(svcs))
	for i := range svcs {
		s := &svcs[i]
		snapshot[s.Namespace+"/"+s.Name] = s
	}
	c.mu.Lock()
	c.svcSnapshot = snapshot
	c.mu.Unlock()

	routes := make([]ingress.Route, 0)
	hosts := make(map[string]struct{})
	clientCAHosts := make(map[string]struct{})
	for i := range svcs {
		s := &svcs[i]
		if s.Expose == nil || s.Expose.Host == "" {
			continue
		}
		port := primaryServicePort(s)
		if port == 0 {
			continue
		}
		routes = append(routes, ingress.Route{
			Host:       s.Expose.Host,
			Namespace:  s.Namespace,
			Service:    s.Name,
			Port:       port,
			Path:       s.Expose.Path,
			AllowCIDRs: parseAllowCIDRs(s.Expose.AllowCIDRs, s.Expose.Host, c.cfg.Logger),
		})
		hosts[s.Expose.Host] = struct{}{}
		if c.cfg.ACME != nil && s.Expose.TLS.IsACME() {
			c.cfg.ACME.Submit(acme.Request{
				Namespace: s.Namespace,
				Name:      s.Name,
				Host:      s.Expose.Host,
			})
		} else if s.Expose.TLS != nil && s.Expose.TLS.Mode == types.ExposeTLSModeManual && s.Expose.TLS.Secret != "" {
			c.applyManualTLS(ctx, s)
		}
		if s.Expose.ClientCert != nil && s.Expose.ClientCert.CASecret != "" {
			c.applyClientCert(ctx, s)
			clientCAHosts[s.Expose.Host] = struct{}{}
		}
	}
	c.cfg.Router.Apply(routes)
	c.pruneClientCAs(clientCAHosts)

	// Log only when the host set changes so steady-state reconciles
	// stay quiet.
	c.mu.Lock()
	changed := !sameSet(c.lastHosts, hosts)
	c.lastHosts = hosts
	c.mu.Unlock()
	if changed {
		c.cfg.Logger.Info("ingress routes applied",
			log.Int("count", len(routes)),
			log.Any("hosts", keys(hosts)))
	}
}

// applyManualTLS loads the Secret referenced by Expose.TLS.Secret and
// pushes its tls.crt / tls.key into the cert store. Idempotent per
// (host, secret-version): subsequent reconcile ticks where the
// underlying Secret hasn't changed skip the parse + push so we're
// not re-doing PEM work every 2 s in the steady state.
//
// Expose.TLS.Secret accepts the three canonical resource-ref shapes
// (see types.ParseResourceRefWithDefaults): bare name, ns/name
// shorthand, FQDN secret ref. Non-secret types are rejected.
//
// Quiet on missing wiring (Secrets / Certs nil): the ingress route
// is still applied so HTTP works; TLS handshakes will surface "no
// cert for host" until the operator notices the missing secret. The
// API-server validator is responsible for catching obvious cast-time
// shapes (mode: manual + empty Secret).
func (c *Controller) applyManualTLS(ctx context.Context, s *types.Service) {
	if c.cfg.Secrets == nil || c.cfg.Certs == nil {
		return
	}
	host := s.Expose.Host

	ref, refErr := types.ParseResourceRefWithDefaults(s.Expose.TLS.Secret, types.ResourceTypeSecret, s.Namespace)
	if refErr != nil {
		c.warnManualOnce(host, "ref-parse", "manual TLS: secret ref parse failed",
			log.Str("host", host),
			log.Str("namespace", s.Namespace),
			log.Str("secret", s.Expose.TLS.Secret),
			log.Err(refErr))
		return
	}
	if ref.Type != types.ResourceTypeSecret {
		c.warnManualOnce(host, "ref-not-secret", "manual TLS: ref is not a secret",
			log.Str("host", host),
			log.Str("namespace", s.Namespace),
			log.Str("secret", s.Expose.TLS.Secret),
			log.Str("refType", string(ref.Type)))
		return
	}
	secret, err := c.cfg.Secrets.Get(ctx, ref.Namespace, ref.Name)
	if err != nil {
		c.warnManualOnce(host, "secret-lookup", "manual TLS: secret lookup failed",
			log.Str("host", host),
			log.Str("namespace", s.Namespace),
			log.Str("secret", ref.Namespace+"/"+ref.Name),
			log.Err(err))
		return
	}
	cert := secret.Data["tls.crt"]
	key := secret.Data["tls.key"]
	if cert == "" || key == "" {
		c.warnManualOnce(host, "missing-keys", "manual TLS: secret missing tls.crt/tls.key data keys",
			log.Str("host", host),
			log.Str("namespace", s.Namespace),
			log.Str("secret", ref.Namespace+"/"+ref.Name))
		return
	}

	// Skip the push when this (host, secret-version) pair already
	// landed in the cert store. Secret.Version monotonically
	// increases on every Update so a rotation forces a re-push.
	c.mu.Lock()
	if prev, ok := c.manualPushed[host]; ok && prev == secret.Version {
		c.mu.Unlock()
		return
	}
	c.manualPushed[host] = secret.Version
	c.mu.Unlock()

	if err := c.cfg.Certs.Set(ctx, host, []byte(cert), []byte(key)); err != nil {
		c.warnManualOnce(host, "cert-set", "manual TLS: cert store Set failed",
			log.Str("host", host),
			log.Str("namespace", s.Namespace),
			log.Str("secret", ref.Namespace+"/"+ref.Name),
			log.Err(err))
		// Clear our cached version so the next tick retries.
		c.mu.Lock()
		delete(c.manualPushed, host)
		c.mu.Unlock()
		return
	}
	// On success: clear any prior warn-dedup entries for this host so
	// the next failure (if there is one) logs immediately again, and
	// emit the success line (which is always logged — it's rare).
	c.clearManualWarns(host)
	c.cfg.Logger.Info("manual TLS: cert loaded from secret",
		log.Str("host", host),
		log.Str("namespace", s.Namespace),
		log.Str("secret", ref.Namespace+"/"+ref.Name),
		log.Int("secretVersion", secret.Version))
}

// applyClientCert resolves Expose.ClientCert.CASecret into an x509 pool
// and pushes it into the shared ClientCAs registry, so the ingress
// listener requires + verifies a client cert for this host. Idempotent per
// (host, caSecret-version): a CA rotation re-pushes, steady state skips the
// PEM parse. A misconfigured CA leaves the host's prior pool in place (or
// absent) and warns — it does NOT silently disable mTLS, which would be a
// fail-open regression on a lockdown control.
func (c *Controller) applyClientCert(ctx context.Context, s *types.Service) {
	if c.cfg.ClientCAs == nil || c.cfg.Secrets == nil {
		return
	}
	host := s.Expose.Host
	cc := s.Expose.ClientCert

	ref, refErr := types.ParseResourceRefWithDefaults(cc.CASecret, types.ResourceTypeSecret, s.Namespace)
	if refErr != nil || ref.Type != types.ResourceTypeSecret {
		c.warnManualOnce(host, "ca-ref", "clientCert: caSecret ref parse failed",
			log.Str("host", host), log.Str("namespace", s.Namespace),
			log.Str("caSecret", cc.CASecret), log.Err(refErr))
		return
	}
	secret, err := c.cfg.Secrets.Get(ctx, ref.Namespace, ref.Name)
	if err != nil {
		c.warnManualOnce(host, "ca-lookup", "clientCert: caSecret lookup failed",
			log.Str("host", host), log.Str("namespace", s.Namespace),
			log.Str("caSecret", ref.Namespace+"/"+ref.Name), log.Err(err))
		return
	}
	pem := secret.Data["ca.crt"]
	if pem == "" {
		c.warnManualOnce(host, "ca-missing", "clientCert: caSecret missing 'ca.crt' data key",
			log.Str("host", host), log.Str("namespace", s.Namespace),
			log.Str("caSecret", ref.Namespace+"/"+ref.Name))
		return
	}

	// Skip the parse + push when this (host, secret-version) already landed.
	c.mu.Lock()
	if prev, ok := c.clientCAPushed[host]; ok && prev == secret.Version {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		c.warnManualOnce(host, "ca-parse", "clientCert: caSecret ca.crt contains no valid PEM certificate",
			log.Str("host", host), log.Str("namespace", s.Namespace),
			log.Str("caSecret", ref.Namespace+"/"+ref.Name))
		return
	}
	c.cfg.ClientCAs.Set(host, pool)
	c.mu.Lock()
	c.clientCAPushed[host] = secret.Version
	c.mu.Unlock()
	c.clearManualWarns(host)
	c.cfg.Logger.Info("clientCert: CA pool loaded from secret",
		log.Str("host", host), log.Str("namespace", s.Namespace),
		log.Str("caSecret", ref.Namespace+"/"+ref.Name),
		log.Int("secretVersion", secret.Version))
}

// pruneClientCAs forgets registry entries (and version cache) for hosts
// that no longer declare a clientCert, so removing clientCert from a
// service disables mTLS for its host on the next reconcile.
func (c *Controller) pruneClientCAs(current map[string]struct{}) {
	if c.cfg.ClientCAs == nil {
		return
	}
	c.mu.Lock()
	var dropped []string
	for h := range c.clientCAHosts {
		if _, ok := current[h]; !ok {
			dropped = append(dropped, h)
			delete(c.clientCAPushed, h)
		}
	}
	c.clientCAHosts = current
	c.mu.Unlock()
	for _, h := range dropped {
		c.cfg.ClientCAs.Forget(h)
		c.cfg.Logger.Info("clientCert: disabled (clientCert removed)", log.Str("host", h))
	}
}

// warnManualOnce emits a Warn-level log no more than once every
// ManualLogRepeat for a given (host, errKind). Subsequent identical
// failures within the window are dropped on the floor — useful because
// the reconciler ticks every 2 s and an unfixed misconfigured Secret
// would otherwise spam the journal.
func (c *Controller) warnManualOnce(host, errKind, msg string, fields ...log.Field) {
	key := host + "::" + errKind
	c.mu.Lock()
	last, seen := c.manualLogged[key]
	now := time.Now()
	if seen && now.Sub(last) < ManualLogRepeat {
		c.mu.Unlock()
		return
	}
	c.manualLogged[key] = now
	c.mu.Unlock()
	c.cfg.Logger.Warn(msg, fields...)
}

// clearManualWarns drops every dedup entry for host. Called on a
// successful push so the operator gets an immediate warning if the
// secret breaks again later (e.g. after a bad rotation).
func (c *Controller) clearManualWarns(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := host + "::"
	for k := range c.manualLogged {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.manualLogged, k)
		}
	}
}

// parseAllowCIDRs parses the source-IP allowlist into networks for the
// route. Entries are validated at cast time; we parse defensively here and
// skip (with a warning) any that fail, rather than dropping the whole route
// — a malformed entry shouldn't silently widen access by yielding an empty
// (allow-all) list, so callers should treat a parse-skip as a tightening.
func parseAllowCIDRs(cidrs []string, host string, logger log.Logger) []*net.IPNet {
	if len(cidrs) == 0 {
		return nil
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil || n == nil {
			if logger != nil {
				logger.Warn("ingress: skipping invalid allowCidrs entry",
					log.Str("host", host), log.Str("cidr", c))
			}
			continue
		}
		out = append(out, n)
	}
	return out
}

func primaryServicePort(s *types.Service) int {
	if len(s.Ports) == 0 {
		return 0
	}
	p := s.Ports[0]
	if p.TargetPort != 0 {
		return p.TargetPort
	}
	return p.Port
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
