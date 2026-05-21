package dns

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/endpoints"
	"github.com/runestack/rune/pkg/networking/localinstances"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"github.com/runestack/rune/pkg/types"
)

// EndpointPublisher fans the orchestrator's endpoint + local-instance
// updates into the OrderedLog-backed publishers from
// pkg/networking/endpoints and pkg/networking/localinstances.
//
// It implements the
// pkg/orchestrator/controllers.EndpointPublisher interface (kept
// loose-coupled to avoid an import cycle: this package only refers to
// pkg/types).
type EndpointPublisher struct {
	endpoints      endpoints.Publisher
	localInstances localinstances.Publisher
	logger         log.Logger
}

// NewEndpointPublisher constructs an EndpointPublisher. Both
// underlying publishers are required.
func NewEndpointPublisher(olog orderedlog.OrderedLog, logger log.Logger) (*EndpointPublisher, error) {
	if err := endpoints.Register(olog); err != nil {
		return nil, fmt.Errorf("dns: register endpoints op: %w", err)
	}
	if err := localinstances.Register(olog); err != nil {
		return nil, fmt.Errorf("dns: register localinstances op: %w", err)
	}
	return &EndpointPublisher{
		endpoints:      endpoints.NewPublisher(olog),
		localInstances: localinstances.NewPublisher(olog),
		logger:         logger,
	}, nil
}

// PublishService implements controllers.EndpointPublisher.
func (p *EndpointPublisher) PublishService(ctx context.Context, service *types.Service, eps []types.Endpoint) error {
	if service == nil {
		return nil
	}
	if len(eps) == 0 {
		return p.endpoints.Delete(ctx, service.Name)
	}
	return p.endpoints.Update(ctx, service.Name, eps)
}

// PublishLocalInstances implements controllers.EndpointPublisher.
func (p *EndpointPublisher) PublishLocalInstances(ctx context.Context, nodeID string, table map[string]types.InstanceIdentity) error {
	if nodeID == "" {
		return nil
	}
	if len(table) == 0 {
		return p.localInstances.Delete(ctx, nodeID)
	}
	return p.localInstances.Update(ctx, nodeID, table)
}

// FuncVIPSource adapts a lookup function to ServiceVIPSource.
type FuncVIPSource struct {
	Fn func(ctx context.Context, serviceID string) (net.IP, error)
}

func (f FuncVIPSource) VIPForService(ctx context.Context, serviceID string) (net.IP, error) {
	if f.Fn == nil {
		return nil, nil
	}
	return f.Fn(ctx, serviceID)
}

// ServiceVIPSource resolves the stable cluster VIP for a service ID.
// The VIP allocator implements this; DNS uses it when the service
// row is missing Service.Discovery.VIP (legacy drift).
type ServiceVIPSource interface {
	VIPForService(ctx context.Context, serviceID string) (net.IP, error)
}

// StoreZone is a ZoneProvider that resolves <svc>.<ns>.rune queries
// against the agent's store.Store. The VIP is read from
// Service.Discovery.VIP when present; otherwise ServiceVIPSource
// (the cluster allocator) is consulted so DNS never depends on
// operators hand-setting discovery.vip in cast YAML.
//
// Lookups are memoized for a short TTL to avoid hammering the store
// on bursts of repeated DNS queries from a single client.
type StoreZone struct {
	store  store.Store
	vips   ServiceVIPSource
	mu     sync.RWMutex
	cache  map[string]storeZoneCacheEntry
	ttl    time.Duration
	logger log.Logger
}

type storeZoneCacheEntry struct {
	ips     []net.IP
	ok      bool
	expires time.Time
}

// NewStoreZone constructs a StoreZone with a 1s lookup cache.
// vip may be nil (store-only lookups).
func NewStoreZone(s store.Store, vip ServiceVIPSource, logger log.Logger) *StoreZone {
	return &StoreZone{
		store:  s,
		vips:   vip,
		cache:  map[string]storeZoneCacheEntry{},
		ttl:    1 * time.Second,
		logger: logger,
	}
}

// LookupA implements ZoneProvider.
func (z *StoreZone) LookupA(ns, name string) ([]net.IP, bool) {
	key := ns + "/" + name
	now := time.Now()
	z.mu.RLock()
	if e, ok := z.cache[key]; ok && now.Before(e.expires) {
		z.mu.RUnlock()
		return e.ips, e.ok
	}
	z.mu.RUnlock()

	var svc types.Service
	err := z.store.Get(context.Background(), types.ResourceTypeService, ns, name, &svc)
	var ips []net.IP
	ok := false
	if err == nil {
		if vip := discoveryVIP(&svc); vip != nil {
			ips = []net.IP{vip}
			ok = true
		} else if z.vips != nil && svc.ID != "" {
			if ip, vipErr := z.vips.VIPForService(context.Background(), svc.ID); vipErr == nil && ip != nil {
				if v4 := ip.To4(); v4 != nil {
					ips = []net.IP{v4}
					ok = true
				}
			}
		}
	}
	z.mu.Lock()
	z.cache[key] = storeZoneCacheEntry{ips: ips, ok: ok, expires: now.Add(z.ttl)}
	z.mu.Unlock()
	return ips, ok
}

func discoveryVIP(svc *types.Service) net.IP {
	if svc == nil || svc.Discovery == nil || svc.Discovery.VIP == "" {
		return nil
	}
	ip := net.ParseIP(svc.Discovery.VIP)
	if ip == nil {
		return nil
	}
	return ip.To4()
}

// FreshnessFromDataplane builds a Freshness implementation from any
// type that exposes an IsFresh() bool method (e.g. dataplane.Subsystem
// once it grows that accessor). When the supplied function is nil,
// the DNS server treats the data plane as always fresh — appropriate
// for dev/standalone mode.
func FreshnessFromDataplane(isFresh func() bool) Freshness {
	if isFresh == nil {
		return AlwaysFresh()
	}
	return funcFreshness(isFresh)
}

type funcFreshness func() bool

func (f funcFreshness) IsFresh() bool { return f() }

// ResolvConfUpstreams returns an UpstreamProvider that re-reads
// /etc/resolv.conf on every call. Loopback servers (e.g. systemd-resolved)
// are filtered to avoid forwarding loops back into our own bind.
func ResolvConfUpstreams() func() []string {
	return func() []string {
		ups, err := parseResolvConf(resolvConfPath)
		if err != nil {
			return nil
		}
		return ups
	}
}
