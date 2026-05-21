// Package dataplane implements the per-node Rune data path (RUNE-041):
// per-VIP TCP/UDP userspace proxies, a kernel-side nftables reconciler
// (Linux only), an endpoint cache backed by the OrderedLog watch
// stream, and Prometheus metrics. The dataplane registers as an Agent
// Subsystem (internal/agent.Subsystem) so its lifecycle is governed by
// the agent.
//
// On startup the subsystem subscribes to the OrderedLog from seq 0,
// hydrates its endpoint cache, then transitions into live-watch mode.
// On watch disconnect it serves the last-known endpoint set for at
// most StaleBudget (default 30s) before failing closed (refusing new
// connections; existing connections continue until torn down).
//
// Listener lifecycle is driven by service registration. The agent's
// main wires a ServiceProvider that knows each service's VIP and port
// list. When a service is registered the subsystem opens TCP and UDP
// listeners on the VIP (or 127.0.0.1 in dev-mode) for each port; when
// it's unregistered listeners drain (TCP) or close (UDP) within
// DrainTimeout (default 30s). Dev-mode is signalled via Agent.Mode().
package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/endpoints"
	"github.com/runestack/rune/pkg/networking/localinstances"
	"github.com/runestack/rune/pkg/networking/policy"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"github.com/runestack/rune/pkg/types"
)

// Defaults can be overridden via Config.
const (
	DefaultStaleBudget       = 30 * time.Second
	DefaultDrainTimeout      = 30 * time.Second
	DefaultReconnectMinDelay = 200 * time.Millisecond
	DefaultReconnectMaxDelay = 5 * time.Second
)

// Mode mirrors agent.Mode without importing it (avoids import cycle if
// the agent ever depends on the dataplane). Production is the
// kernel-DNAT-and-real-VIP path; Dev is the loopback-listener path
// used by laptop development on macOS.
type Mode int

const (
	ModeProduction Mode = iota
	ModeDev
)

func (m Mode) String() string {
	if m == ModeDev {
		return "dev"
	}
	return "production"
}

// ServiceProvider is the dataplane's read-only view of the service
// catalog. The dataplane needs each registered service's VIP + port
// list to open listeners; it doesn't care about everything else on
// the Service spec. The orchestrator wires the real provider in a
// follow-up; for now main.go uses an in-memory provider populated by
// the public Register / Unregister calls below.
type ServiceProvider interface {
	Lookup(serviceID string) (*types.Service, bool)
}

// NodeIDProvider returns the local node's stable identity. Used for
// localityPreference scoring (prefer-local, local-only).
type NodeIDProvider interface {
	NodeID() string
}

// staticNodeID is a tiny adapter for callers that already have a
// string identity.
type staticNodeID struct{ id string }

func (s staticNodeID) NodeID() string { return s.id }

// StaticNodeID wraps a string node ID into a NodeIDProvider.
func StaticNodeID(id string) NodeIDProvider { return staticNodeID{id: id} }

// Config bundles dataplane construction parameters.
type Config struct {
	// OrderedLog is the source of truth for endpoint mutations.
	// Required.
	OrderedLog orderedlog.OrderedLog

	// Services lets the dataplane look up VIP + ports for a service
	// referenced by an endpoints update. Optional; service registration
	// is driven by Store watch when set.
	Services ServiceProvider

	// Store watches service rows and opens per-VIP listeners. Required
	// for production cluster networking on single-node runed.
	Store store.Store

	// VIPResolver supplies cluster VIPs when a service row is missing
	// discovery.vip (legacy drift). Typically the VIP allocator.
	VIPResolver VIPResolver

	// Node identifies this node for localityPreference. Required for
	// "prefer-local" / "local-only" semantics; in its absence those
	// modes degrade to "no preference".
	Node NodeIDProvider

	// Mode is "production" (default) or "dev". Dev disables nftables
	// and listens on 127.0.0.1 instead of the real VIP.
	Mode Mode

	// ReservedHostPorts are host ports the dataplane must NOT open VIP
	// listeners on. On an edge node the ingress owns :80/:443 with a
	// 0.0.0.0 wildcard bind; a <vip>:80 listener collides with it and
	// fails the whole ingress subsystem. Services exposed on these
	// ports are reached via the ingress, not the VIP.
	ReservedHostPorts []int

	// Logger; defaults to the global logger with component "dataplane".
	Logger log.Logger

	// StaleBudget is the time a watch disconnect can persist before
	// the proxy starts refusing new connections. Defaults to 30s.
	StaleBudget time.Duration

	// DrainTimeout is the connection-drain deadline applied when an
	// endpoint is removed or a listener is being closed. Defaults to 30s.
	DrainTimeout time.Duration
}

// Subsystem is the per-node dataplane. It implements the Subsystem
// interface used by internal/agent (Name / Start / Ready / Stop) but
// does not import the agent package directly to avoid cycles.
type Subsystem struct {
	cfg     Config
	log     log.Logger
	cache   *Cache
	proxy   *ProxyManager
	nfm     nftablesManager
	metrics *Metrics

	// Network policy state (RUNE-064).
	policyMu       sync.RWMutex
	policies       map[string]*policy.Compiled
	localInstances *policy.LocalInstancesTable

	mu      sync.Mutex
	started bool
	stopped bool
	readyCh chan struct{}
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// last successful watch event timestamp; the staleness check uses
	// this to decide when to start failing closed.
	lastEventMu sync.RWMutex
	lastEvent   time.Time

	// Service VIP listener bookkeeping (production mode).
	vipHost       *localVIPHost
	svcRegMu      sync.Mutex
	svcRegistered map[string]string // serviceID -> VIP
	svcWg         sync.WaitGroup
}

// New constructs a Subsystem. It registers the endpoints op types
// against cfg.OrderedLog (idempotent), but does not start any
// goroutines until Start is called.
func New(cfg Config) (*Subsystem, error) {
	if cfg.OrderedLog == nil {
		return nil, errors.New("dataplane: nil OrderedLog")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger().WithComponent("dataplane")
	} else {
		cfg.Logger = cfg.Logger.WithComponent("dataplane")
	}
	if cfg.StaleBudget <= 0 {
		cfg.StaleBudget = DefaultStaleBudget
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	if err := endpoints.Register(cfg.OrderedLog); err != nil {
		return nil, fmt.Errorf("dataplane: register endpoints op: %w", err)
	}
	if err := localinstances.Register(cfg.OrderedLog); err != nil {
		return nil, fmt.Errorf("dataplane: register local_instances op: %w", err)
	}
	nodeID := ""
	if cfg.Node != nil {
		nodeID = cfg.Node.NodeID()
	}
	m := newMetrics()
	cache := newCache()
	s := &Subsystem{
		cfg:            cfg,
		log:            cfg.Logger,
		cache:          cache,
		metrics:        m,
		nfm:            newNFTables(cfg.Mode, cfg.Logger),
		readyCh:        make(chan struct{}),
		policies:       make(map[string]*policy.Compiled),
		localInstances: policy.NewLocalInstancesTable(nodeID),
		vipHost:        newLocalVIPHost(),
		svcRegistered:  make(map[string]string),
	}
	s.proxy = newProxyManager(cfg, cache, m, s.fresh, s.evaluatePolicy)
	return s, nil
}

// Name implements the agent.Subsystem interface.
func (s *Subsystem) Name() string { return "dataplane" }

// Ready returns a channel closed once the subsystem has hydrated its
// cache from the OrderedLog and is serving live updates.
func (s *Subsystem) Ready() <-chan struct{} { return s.readyCh }

// Cache returns the live endpoint cache. Exposed for diagnostics +
// tests; callers should not mutate the returned snapshot.
func (s *Subsystem) Cache() *Cache { return s.cache }

// Metrics returns the dataplane's Prometheus collector set so the
// agent's HTTP server can register them.
func (s *Subsystem) Metrics() *Metrics { return s.metrics }

// RegisterService opens listeners for svc on this node. Idempotent
// per service ID; mutating fields (port list, VIP) cause the
// listener set to be reconciled. Safe to call before Start.
func (s *Subsystem) RegisterService(svc *types.Service) error {
	if svc == nil {
		return errors.New("dataplane: nil service")
	}
	// Compile and store policy before opening listeners so the very
	// first connection sees the active rule set.
	compiled := policy.Compile(svc)
	s.policyMu.Lock()
	if compiled == nil {
		delete(s.policies, svc.ID)
	} else {
		s.policies[svc.ID] = compiled
	}
	s.policyMu.Unlock()
	if compiled != nil {
		s.metrics.setPolicyRules(svc.ID, svc.Namespace, compiled.IngressRuleCount()+compiled.EgressRuleCount())
	} else {
		s.metrics.setPolicyRules(svc.ID, svc.Namespace, 0)
	}
	s.metrics.setPolicyLastSeq(svc.ID, svc.Namespace, time.Now().Unix())
	return s.proxy.Register(svc)
}

// UnregisterService closes listeners for serviceID. Connections drain
// for up to DrainTimeout. Idempotent.
func (s *Subsystem) UnregisterService(serviceID string) {
	s.policyMu.Lock()
	delete(s.policies, serviceID)
	s.policyMu.Unlock()
	s.proxy.Unregister(serviceID)
}

// LocalInstances returns the agent's IP -> identity table for
// diagnostics and tests.
func (s *Subsystem) LocalInstances() *policy.LocalInstancesTable { return s.localInstances }

// evaluatePolicy is the closure handed to the proxy manager. It
// resolves the source IP to identity via the LocalInstances table
// then evaluates against the destination service's compiled policy.
func (s *Subsystem) evaluatePolicy(serviceID string, srcIP net.IP, port int, proto string) policy.Result {
	s.policyMu.RLock()
	c := s.policies[serviceID]
	s.policyMu.RUnlock()
	peer := s.localInstances.PeerInfoFor(srcIP)
	return c.EvaluateIngress(peer, port, proto)
}

// PolicyFor returns the compiled policy for serviceID for diagnostics.
func (s *Subsystem) PolicyFor(serviceID string) *policy.Compiled {
	s.policyMu.RLock()
	defer s.policyMu.RUnlock()
	return s.policies[serviceID]
}

// Start begins the watch loop and reconciler. Returns once Ready
// becomes ready or the context is cancelled.
func (s *Subsystem) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("dataplane: already started")
	}
	s.started = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()

	// Hydrate cache from a snapshot before going live so the watch
	// loop can resume from a known seq without missing events.
	startSeq, err := s.hydrate(ctx)
	if err != nil {
		return fmt.Errorf("dataplane: hydrate: %w", err)
	}
	s.markFresh()

	close(s.readyCh)

	if s.cfg.Store != nil {
		if err := s.reconcileServicesFromStore(ctx); err != nil {
			s.log.Warn("Dataplane initial service reconcile had errors", log.Err(err))
		}
		watchCh, werr := s.cfg.Store.Watch(runCtx, types.ResourceTypeService, "")
		if werr != nil {
			return fmt.Errorf("dataplane: service watch: %w", werr)
		}
		s.svcWg.Add(1)
		go s.runServiceWatch(runCtx, watchCh)
	}

	s.wg.Add(1)
	go s.watchLoop(runCtx, startSeq)

	s.log.Info("dataplane started",
		log.Str("mode", s.cfg.Mode.String()),
		log.F("from_seq", startSeq),
		log.Duration("stale_budget", s.cfg.StaleBudget),
	)
	return nil
}

// Stop drains all listeners and shuts down the watch loop. Honors
// ctx for an upper bound on drain time.
func (s *Subsystem) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.teardownServiceVIPs()
	s.proxy.Shutdown(ctx)
	s.nfm.Close()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		s.svcWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	s.log.Info("dataplane stopped")
	return nil
}

// hydrate reads a snapshot, populates the cache for every endpoints/
// key, and returns the seq from which the watch loop should resume.
func (s *Subsystem) hydrate(ctx context.Context) (uint64, error) {
	snap, seq, err := s.cfg.OrderedLog.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	defer snap.Close()
	if err := snap.Range([]byte("endpoints/"), func(_, value []byte) error {
		var se types.ServiceEndpoints
		if err := decodePayload(value, &se); err != nil {
			s.log.Warn("hydrate: skip malformed endpoints record", log.Err(err))
			return nil
		}
		s.cache.Set(se.ServiceID, se.Endpoints)
		s.metrics.observeEndpointSet(se.ServiceID, len(se.Endpoints))
		return nil
	}); err != nil {
		return 0, err
	}
	if err := snap.Range([]byte("local_instances/"), func(_, value []byte) error {
		var li types.LocalInstances
		if err := decodeJSON(value, &li); err != nil {
			s.log.Warn("hydrate: skip malformed local_instances record", log.Err(err))
			return nil
		}
		n := s.localInstances.Apply(li)
		s.metrics.setLocalInstances(n)
		return nil
	}); err != nil {
		return 0, err
	}
	return seq, nil
}

// watchLoop subscribes to OrderedLog from startSeq+1 (live) and
// updates the cache. On disconnect it reconnects with backoff and
// will issue a fresh hydrate on ErrCompacted.
func (s *Subsystem) watchLoop(ctx context.Context, fromSeq uint64) {
	defer s.wg.Done()
	delay := s.cfg.reconnectDelay()
	cur := fromSeq
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		ch, err := s.cfg.OrderedLog.Watch(ctx, cur)
		if errors.Is(err, orderedlog.ErrCompacted) {
			s.log.Warn("watch: compacted, re-hydrating")
			newSeq, herr := s.hydrate(ctx)
			if herr != nil {
				s.log.Error("watch: re-hydrate failed", log.Err(herr))
				s.staleSleep(ctx, delay.next())
				continue
			}
			cur = newSeq
			s.markFresh()
			continue
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			s.log.Warn("watch: subscribe failed", log.Err(err))
			s.staleSleep(ctx, delay.next())
			continue
		}
		delay.reset()
		stop := s.consume(ctx, ch, &cur)
		if stop {
			return
		}
		// Channel closed (server-side drop). Loop and reconnect.
	}
}

// consume drains ch, applying mutations to the cache and updating
// the freshness clock. Returns true when ctx is done.
func (s *Subsystem) consume(ctx context.Context, ch <-chan orderedlog.Event, cur *uint64) bool {
	for {
		select {
		case <-ctx.Done():
			return true
		case ev, ok := <-ch:
			if !ok {
				return false
			}
			s.applyEvent(ev)
			*cur = ev.Seq
			s.markFresh()
		}
	}
}

func (s *Subsystem) applyEvent(ev orderedlog.Event) {
	for _, m := range ev.Mutations {
		switch {
		case endpoints.IsEndpointsMutation(m):
			s.applyEndpointsMutation(m)
		case localinstances.IsLocalInstancesMutation(m):
			s.applyLocalInstancesMutation(m)
		}
	}
}

func (s *Subsystem) applyEndpointsMutation(m orderedlog.Mutation) {
	switch m.Kind {
	case orderedlog.MutationDelete:
		s.cache.Delete(m.Name)
		s.metrics.observeEndpointSet(m.Name, 0)
		s.log.Debug("endpoints deleted", log.Str("service_id", m.Name))
	case orderedlog.MutationPut:
		se, err := endpoints.DecodePayload(m)
		if err != nil {
			s.log.Warn("apply: decode endpoints payload failed", log.Err(err))
			return
		}
		s.cache.Set(se.ServiceID, se.Endpoints)
		s.metrics.observeEndpointSet(se.ServiceID, len(se.Endpoints))
		s.log.Debug("endpoints updated",
			log.Str("service_id", se.ServiceID),
			log.Int("count", len(se.Endpoints)),
		)
	}
}

func (s *Subsystem) applyLocalInstancesMutation(m orderedlog.Mutation) {
	switch m.Kind {
	case orderedlog.MutationDelete:
		n := s.localInstances.Remove(m.Name)
		s.metrics.setLocalInstances(n)
		s.log.Debug("local_instances deleted", log.Str("node_id", m.Name))
	case orderedlog.MutationPut:
		li, err := localinstances.DecodePayload(m)
		if err != nil {
			s.log.Warn("apply: decode local_instances payload failed", log.Err(err))
			return
		}
		n := s.localInstances.Apply(li)
		s.metrics.setLocalInstances(n)
		s.log.Debug("local_instances updated",
			log.Str("node_id", li.NodeID),
			log.Int("count", len(li.Instances)),
		)
	}
}

// fresh reports whether the dataplane has received a watch event
// (or finished hydration) within StaleBudget. Used by the proxy to
// fail closed on extended watch disconnects.
func (s *Subsystem) fresh() bool {
	s.lastEventMu.RLock()
	defer s.lastEventMu.RUnlock()
	if s.lastEvent.IsZero() {
		return false
	}
	return time.Since(s.lastEvent) <= s.cfg.StaleBudget
}

func (s *Subsystem) markFresh() {
	now := time.Now()
	s.lastEventMu.Lock()
	s.lastEvent = now
	s.lastEventMu.Unlock()
	s.metrics.setWatchLag(0)
}

// staleSleep waits d while watching ctx and updating the watch-lag
// metric so operators can see the disconnect.
func (s *Subsystem) staleSleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case <-tick.C:
			s.lastEventMu.RLock()
			last := s.lastEvent
			s.lastEventMu.RUnlock()
			if !last.IsZero() {
				s.metrics.setWatchLag(time.Since(last).Seconds())
			}
		}
	}
}

// reconnectDelay returns an exponential backoff helper.
func (c Config) reconnectDelay() *backoff {
	min := DefaultReconnectMinDelay
	max := DefaultReconnectMaxDelay
	return &backoff{cur: min, min: min, max: max}
}

type backoff struct {
	cur, min, max time.Duration
}

func (b *backoff) next() time.Duration {
	d := b.cur
	b.cur *= 2
	if b.cur > b.max {
		b.cur = b.max
	}
	return d
}
func (b *backoff) reset() { b.cur = b.min }
