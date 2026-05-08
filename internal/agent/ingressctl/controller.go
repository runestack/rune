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
}

// New constructs a Controller. Logger and ReconcilePeriod default.
func New(cfg Config) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger().WithComponent("ingressctl")
	}
	if cfg.ReconcilePeriod <= 0 {
		cfg.ReconcilePeriod = 2 * time.Second
	}
	return &Controller{cfg: cfg, lastHosts: map[string]struct{}{}}
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
