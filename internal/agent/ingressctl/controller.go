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
	// Logger is the structured logger. Defaults to "ingressctl".
	Logger log.Logger
	// ReconcilePeriod is how often the controller rebuilds the
	// route table from the store as a safety net. Defaults to 2s.
	ReconcilePeriod time.Duration
}

// Controller reconciles ingress routes from the service store and
// implements ingress.UpstreamResolver against the dataplane cache.
type Controller struct {
	cfg Config

	mu        sync.RWMutex
	lastHosts map[string]struct{} // for log noise control

	// manualPushed tracks the (host, secret-version) pair last pushed
	// to the cert store via manual-mode handling. Lets the per-tick
	// reconcile re-push when the underlying secret changes (so secret
	// rotation flows through automatically) but skip the work when
	// nothing changed (so we're not re-parsing a PEM every 2s).
	manualPushed map[string]int
}

// New constructs a Controller. Logger and ReconcilePeriod default.
func New(cfg Config) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger().WithComponent("ingressctl")
	}
	if cfg.ReconcilePeriod <= 0 {
		cfg.ReconcilePeriod = 2 * time.Second
	}
	return &Controller{
		cfg:          cfg,
		lastHosts:    map[string]struct{}{},
		manualPushed: map[string]int{},
	}
}

// Resolve implements ingress.UpstreamResolver. It returns the first
// healthy endpoint for the named service from the dataplane cache,
// dialable as "ip:port". The dataplane Cache is keyed by service
// name today (see internal/agent/dns/wire.go EndpointPublisher),
// so namespace is accepted for interface compatibility but unused.
// The port argument is used as a fallback when the cached endpoint
// has no recorded port.
func (c *Controller) Resolve(namespace, service string, port int) (string, bool) {
	if c.cfg.Cache == nil || service == "" {
		return "", false
	}
	eps, ok := c.cfg.Cache.Healthy(service)
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
	routes := make([]ingress.Route, 0)
	hosts := make(map[string]struct{})
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
			Host:      s.Expose.Host,
			Namespace: s.Namespace,
			Service:   s.Name,
			Port:      port,
			Path:      s.Expose.Path,
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
	}
	c.cfg.Router.Apply(routes)

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
		c.cfg.Logger.Warn("manual TLS: secret ref parse failed",
			log.Str("host", host),
			log.Str("namespace", s.Namespace),
			log.Str("secret", s.Expose.TLS.Secret),
			log.Err(refErr))
		return
	}
	if ref.Type != types.ResourceTypeSecret {
		c.cfg.Logger.Warn("manual TLS: ref is not a secret",
			log.Str("host", host),
			log.Str("namespace", s.Namespace),
			log.Str("secret", s.Expose.TLS.Secret),
			log.Str("refType", string(ref.Type)))
		return
	}
	secret, err := c.cfg.Secrets.Get(ctx, ref.Namespace, ref.Name)
	if err != nil {
		c.cfg.Logger.Warn("manual TLS: secret lookup failed",
			log.Str("host", host),
			log.Str("namespace", s.Namespace),
			log.Str("secret", ref.Namespace+"/"+ref.Name),
			log.Err(err))
		return
	}
	cert := secret.Data["tls.crt"]
	key := secret.Data["tls.key"]
	if cert == "" || key == "" {
		c.cfg.Logger.Warn("manual TLS: secret missing tls.crt/tls.key data keys",
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
		c.cfg.Logger.Warn("manual TLS: cert store Set failed",
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
	c.cfg.Logger.Info("manual TLS: cert loaded from secret",
		log.Str("host", host),
		log.Str("namespace", s.Namespace),
		log.Str("secret", ref.Namespace+"/"+ref.Name),
		log.Int("secretVersion", secret.Version))
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
