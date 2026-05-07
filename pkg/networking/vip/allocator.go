// Package vip implements the cluster VIP allocator.
//
// Every Service in Rune gets a stable virtual IP for its lifetime. The
// allocator owns the cluster's CIDR, allocates IPs out of a free list,
// and returns IPs to the free list after a 60-second cooldown to
// drain stale clients.
//
// The allocator is the single writer of the ClusterNetwork resource.
// All state changes flow through orderedlog.Propose, so a Phase 2
// Raft-backed control plane will see the same state changes in the
// same order without code changes here. The allocator never touches
// Badger directly; the orderedlog seam lint enforces this.
package vip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"github.com/runestack/rune/pkg/types"
)

// Reserved Badger key prefix for cluster network state. The
// orderedlog seam lint forbids direct writes to network/ from outside
// this package and pkg/store/orderedlog.
const (
	keyClusterNetwork = "network/cluster"
)

// Op type constants. Wire-stable: changing the string breaks replay
// of old persisted events.
const (
	OpTypeBootstrapClusterNetwork = "vip.bootstrap"
	OpTypeAllocateVIP             = "vip.allocate"
	OpTypeReleaseVIP              = "vip.release"
)

// Sentinel errors.
var (
	// ErrCIDRExhausted is returned by Allocate when no IPs are free.
	ErrCIDRExhausted = errors.New("vip: cluster CIDR exhausted; expand --cluster-cidr or release unused services")

	// ErrAlreadyBootstrapped is returned by Bootstrap if the cluster
	// network already exists with a different CIDR.
	ErrAlreadyBootstrapped = errors.New("vip: cluster network already bootstrapped with a different CIDR; reset cluster to change")

	// ErrInvalidCIDR is returned for unparseable, non-IPv4, or
	// public-range CIDRs.
	ErrInvalidCIDR = errors.New("vip: invalid CIDR (must be a private IPv4 range)")

	// ErrCIDRCollision is returned by Bootstrap when the configured
	// CIDR overlaps an existing host route.
	ErrCIDRCollision = errors.New("vip: cluster CIDR overlaps an existing host route")

	// ErrServiceNotAllocated is returned by Release for a serviceID
	// with no current allocation.
	ErrServiceNotAllocated = errors.New("vip: service has no VIP allocation")
)

// ReleaseCooldown is how long an IP waits in the pending-release
// queue before returning to the free list. Exposed for tests.
var ReleaseCooldown = 60 * time.Second

// Options configure an Allocator.
type Options struct {
	// CIDR is the desired service CIDR at bootstrap. Ignored once the
	// cluster is bootstrapped (the persisted CIDR wins).
	CIDR string

	// SkipRouteCheck disables the netlink CIDR-collision check at
	// Bootstrap. Used in tests and on platforms without netlink.
	SkipRouteCheck bool

	Logger log.Logger
}

// Allocator owns the ClusterNetwork resource and the in-memory
// pending-release queue. Safe for concurrent use.
type Allocator struct {
	olog orderedlog.OrderedLog
	log  log.Logger
	opts Options

	// in-memory mirror of committed state, protected by mu
	mu      sync.RWMutex
	state   types.ClusterNetwork
	loaded  bool

	// Pending releases waiting for the cooldown window to elapse.
	// IP -> release-time. The release goroutine flushes them via Propose.
	pendingMu sync.Mutex
	pending   map[string]time.Time

	// Lifecycle.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New constructs an Allocator backed by the given OrderedLog. It
// registers the allocator's Op types so subsequent Propose / Watch
// replay works. Call Bootstrap() exactly once at startup.
func New(olog orderedlog.OrderedLog, opts Options) (*Allocator, error) {
	if opts.Logger == nil {
		opts.Logger = log.GetDefaultLogger().WithComponent("vip.allocator")
	}
	a := &Allocator{
		olog:    olog,
		log:     opts.Logger,
		opts:    opts,
		pending: make(map[string]time.Time),
	}
	if err := a.register(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Allocator) register() error {
	pairs := []struct {
		t string
		u orderedlog.OpUnmarshaler
		f orderedlog.Applier
	}{
		{OpTypeBootstrapClusterNetwork, unmarshalBootstrap, a.applyBootstrap},
		{OpTypeAllocateVIP, unmarshalAllocate, a.applyAllocate},
		{OpTypeReleaseVIP, unmarshalRelease, a.applyRelease},
	}
	for _, p := range pairs {
		if err := a.olog.Register(p.t, p.f, p.u); err != nil &&
			!errors.Is(err, orderedlog.ErrAlreadyRegistered) {
			return fmt.Errorf("vip: register %s: %w", p.t, err)
		}
	}
	return nil
}

// Bootstrap ensures the cluster network exists. If it does not, the
// allocator validates the configured CIDR (private range + no host
// route collision unless SkipRouteCheck) and Proposes a
// BootstrapClusterNetwork op. Idempotent: re-running with the same
// CIDR is a no-op; running with a different CIDR returns
// ErrAlreadyBootstrapped.
//
// Bootstrap also starts the background release-cooldown worker.
func (a *Allocator) Bootstrap(ctx context.Context) error {
	if err := a.loadState(ctx); err != nil {
		return err
	}

	a.mu.RLock()
	have := a.state.CIDR
	a.mu.RUnlock()

	desired := a.opts.CIDR
	if have != "" {
		if desired != "" && have != desired {
			return fmt.Errorf("%w (have=%s want=%s)", ErrAlreadyBootstrapped, have, desired)
		}
		// Already bootstrapped; nothing to do beyond starting the
		// background worker.
		a.startBackground()
		return nil
	}

	if desired == "" {
		return fmt.Errorf("vip: no CIDR configured and no existing ClusterNetwork found")
	}
	ipnet, err := validatePrivateCIDR(desired)
	if err != nil {
		return err
	}
	if !a.opts.SkipRouteCheck {
		if err := checkRouteCollision(ipnet); err != nil {
			return fmt.Errorf("%w: %v", ErrCIDRCollision, err)
		}
	}

	if _, err := a.olog.Propose(ctx, &bootstrapOp{CIDR: desired}); err != nil {
		return fmt.Errorf("vip: propose bootstrap: %w", err)
	}
	a.log.Info("ClusterNetwork bootstrapped", log.Str("cidr", desired))
	a.startBackground()
	return nil
}

// Allocate returns the (possibly already-allocated) VIP for serviceID.
// Calls are idempotent: a second Allocate for the same serviceID
// returns the existing IP without touching the free list.
func (a *Allocator) Allocate(ctx context.Context, serviceID string) (net.IP, error) {
	if serviceID == "" {
		return nil, fmt.Errorf("vip: empty serviceID")
	}
	a.mu.RLock()
	if existing, ok := a.state.AllocatedVIPs[serviceID]; ok {
		a.mu.RUnlock()
		return net.ParseIP(existing), nil
	}
	a.mu.RUnlock()

	if _, err := a.olog.Propose(ctx, &allocateOp{ServiceID: serviceID}); err != nil {
		return nil, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	ip, ok := a.state.AllocatedVIPs[serviceID]
	if !ok {
		// Should be impossible: Propose returned without error but
		// the applier did not record the allocation.
		return nil, fmt.Errorf("vip: allocation for %q vanished after commit", serviceID)
	}
	return net.ParseIP(ip), nil
}

// Release schedules the VIP for serviceID to return to the free list
// after the ReleaseCooldown elapses. Returns immediately;
// ErrServiceNotAllocated if the service has no allocation.
//
// The actual free-list update is performed by the background worker
// which Proposes a ReleaseVIP op once the cooldown window passes.
// This keeps Release synchronous-feeling for callers without
// blocking shutdown.
func (a *Allocator) Release(serviceID string) error {
	a.mu.RLock()
	ip, ok := a.state.AllocatedVIPs[serviceID]
	a.mu.RUnlock()
	if !ok {
		return ErrServiceNotAllocated
	}
	a.pendingMu.Lock()
	if _, dup := a.pending[ip]; !dup {
		a.pending[ip] = time.Now().Add(ReleaseCooldown)
	}
	a.pendingMu.Unlock()
	a.log.Info("VIP scheduled for release",
		log.Str("service_id", serviceID), log.Str("vip", ip),
		log.Duration("cooldown", ReleaseCooldown))
	return nil
}

// Status returns a copy of the current ClusterNetwork plus the count
// of pending-release IPs (not yet returned to the free list).
func (a *Allocator) Status() (types.ClusterNetwork, int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := types.ClusterNetwork{
		CIDR:          a.state.CIDR,
		AllocatedVIPs: make(map[string]string, len(a.state.AllocatedVIPs)),
		FreeList:      append([]string(nil), a.state.FreeList...),
	}
	for k, v := range a.state.AllocatedVIPs {
		out.AllocatedVIPs[k] = v
	}
	a.pendingMu.Lock()
	pendingCount := len(a.pending)
	a.pendingMu.Unlock()
	return out, pendingCount
}

// Close stops the background worker. The OrderedLog is owned by the
// caller and is NOT closed here.
func (a *Allocator) Close() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	return nil
}

// loadState is a no-op now: the allocator's in-memory mirror is
// driven exclusively by Applier invocations. Bootstrap() always runs
// at startup and its Applier reads the persisted ClusterNetwork (if
// any) into a.state in either the fresh-cluster or already-bootstrapped
// branch.
func (a *Allocator) loadState(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loaded = true
	return nil
}

func (a *Allocator) startBackground() {
	if a.cancel != nil {
		return
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.wg.Add(1)
	go a.releaseWorker()
}

func (a *Allocator) releaseWorker() {
	defer a.wg.Done()
	tick := time.NewTicker(ReleaseCooldown / 6)
	if ReleaseCooldown < 6*time.Second {
		tick = time.NewTicker(time.Second)
	}
	defer tick.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case now := <-tick.C:
			a.flushReady(now)
		}
	}
}

func (a *Allocator) flushReady(now time.Time) {
	a.pendingMu.Lock()
	var ready []string
	for ip, t := range a.pending {
		if !now.Before(t) {
			ready = append(ready, ip)
		}
	}
	for _, ip := range ready {
		delete(a.pending, ip)
	}
	a.pendingMu.Unlock()
	for _, ip := range ready {
		if _, err := a.olog.Propose(a.ctx, &releaseOp{IP: ip}); err != nil {
			a.log.Warn("vip: release propose failed; will retry",
				log.Str("vip", ip), log.Err(err))
			// Re-queue so we try again next tick.
			a.pendingMu.Lock()
			a.pending[ip] = time.Now().Add(time.Second)
			a.pendingMu.Unlock()
		}
	}
}

// --- appliers --------------------------------------------------------

func (a *Allocator) loadStateTxn(tx orderedlog.Txn) (types.ClusterNetwork, error) {
	var cn types.ClusterNetwork
	raw, err := tx.Get([]byte(keyClusterNetwork))
	if err != nil {
		// Missing == not bootstrapped yet. Return zero value.
		return cn, nil
	}
	if len(raw) == 0 {
		return cn, nil
	}
	if err := json.Unmarshal(raw, &cn); err != nil {
		return cn, fmt.Errorf("vip: decode cluster network: %w", err)
	}
	return cn, nil
}

func saveStateTxn(tx orderedlog.Txn, cn types.ClusterNetwork) ([]orderedlog.Mutation, error) {
	payload, err := json.Marshal(cn)
	if err != nil {
		return nil, err
	}
	if err := tx.Set([]byte(keyClusterNetwork), payload); err != nil {
		return nil, err
	}
	return []orderedlog.Mutation{{
		Kind:         orderedlog.MutationPut,
		ResourceType: "clusternetwork",
		Name:         types.ClusterNetworkName,
		Payload:      payload,
	}}, nil
}

func (a *Allocator) applyBootstrap(tx orderedlog.Txn, op orderedlog.Op) ([]orderedlog.Mutation, error) {
	bo := op.(*bootstrapOp)
	cur, err := a.loadStateTxn(tx)
	if err != nil {
		return nil, err
	}
	if cur.CIDR != "" {
		if cur.CIDR != bo.CIDR {
			return nil, ErrAlreadyBootstrapped
		}
		// Already bootstrapped with same CIDR -> idempotent no-op,
		// but still hydrate the in-memory mirror so subsequent calls
		// (Allocate, Status) see the persisted state.
		a.mu.Lock()
		a.state = cur
		a.mu.Unlock()
		return nil, nil
	}
	cur.CIDR = bo.CIDR
	cur.AllocatedVIPs = map[string]string{}
	cur.FreeList = nil
	muts, err := saveStateTxn(tx, cur)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.state = cur
	a.mu.Unlock()
	return muts, nil
}

func (a *Allocator) applyAllocate(tx orderedlog.Txn, op orderedlog.Op) ([]orderedlog.Mutation, error) {
	ao := op.(*allocateOp)
	cur, err := a.loadStateTxn(tx)
	if err != nil {
		return nil, err
	}
	if cur.CIDR == "" {
		return nil, fmt.Errorf("vip: ClusterNetwork not bootstrapped")
	}
	if cur.AllocatedVIPs == nil {
		cur.AllocatedVIPs = map[string]string{}
	}
	if _, ok := cur.AllocatedVIPs[ao.ServiceID]; ok {
		// Idempotent: re-allocation for the same service is a no-op.
		return nil, nil
	}

	ip, freeList, err := pickIP(cur.CIDR, cur.FreeList, cur.AllocatedVIPs)
	if err != nil {
		return nil, err
	}
	cur.FreeList = freeList
	cur.AllocatedVIPs[ao.ServiceID] = ip.String()

	muts, err := saveStateTxn(tx, cur)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.state = cur
	a.mu.Unlock()
	return muts, nil
}

func (a *Allocator) applyRelease(tx orderedlog.Txn, op orderedlog.Op) ([]orderedlog.Mutation, error) {
	ro := op.(*releaseOp)
	cur, err := a.loadStateTxn(tx)
	if err != nil {
		return nil, err
	}
	// Find serviceID owning this IP and drop it.
	var owner string
	for sid, vip := range cur.AllocatedVIPs {
		if vip == ro.IP {
			owner = sid
			break
		}
	}
	if owner == "" {
		// Already released or unknown IP. Idempotent no-op.
		return nil, nil
	}
	delete(cur.AllocatedVIPs, owner)
	cur.FreeList = append(cur.FreeList, ro.IP)

	muts, err := saveStateTxn(tx, cur)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.state = cur
	a.mu.Unlock()
	return muts, nil
}

// pickIP returns an IP to allocate next, drained from freeList if
// possible or carved from the CIDR otherwise. Reserves the network
// address, broadcast address, and the gateway (.1) per convention.
func pickIP(cidr string, freeList []string, allocated map[string]string) (net.IP, []string, error) {
	if len(freeList) > 0 {
		ip := freeList[0]
		return net.ParseIP(ip), freeList[1:], nil
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, freeList, fmt.Errorf("%w: %v", ErrInvalidCIDR, err)
	}
	// Build inverse set: in-use IPs.
	used := make(map[string]struct{}, len(allocated))
	for _, ip := range allocated {
		used[ip] = struct{}{}
	}
	// Iterate from the gateway+1 (.2) to the broadcast-1.
	netIP := ipnet.IP.To4()
	if netIP == nil {
		return nil, freeList, fmt.Errorf("%w: not IPv4", ErrInvalidCIDR)
	}
	mask := ipnet.Mask
	bcast := make(net.IP, 4)
	for i := range netIP {
		bcast[i] = netIP[i] | ^mask[i]
	}
	cur := dupIP(netIP)
	incIP(cur)             // skip network address (.0)
	incIP(cur)             // skip gateway (.1)
	for !cur.Equal(bcast) {
		s := cur.String()
		if _, inUse := used[s]; !inUse {
			return dupIP(cur), freeList, nil
		}
		incIP(cur)
	}
	return nil, freeList, ErrCIDRExhausted
}

func dupIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	return out
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] != 0 {
			return
		}
	}
}

// --- validation ------------------------------------------------------

func validatePrivateCIDR(cidr string) (*net.IPNet, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCIDR, err)
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("%w: only IPv4 supported in v1", ErrInvalidCIDR)
	}
	if !isPrivateOrCGNAT(ip4) {
		return nil, fmt.Errorf("%w: %s is not RFC1918 / RFC6598", ErrInvalidCIDR, cidr)
	}
	return ipnet, nil
}

func isPrivateOrCGNAT(ip net.IP) bool {
	private := []*net.IPNet{
		mustCIDR("10.0.0.0/8"),
		mustCIDR("172.16.0.0/12"),
		mustCIDR("192.168.0.0/16"),
		mustCIDR("100.64.0.0/10"), // RFC 6598 CGNAT
	}
	for _, p := range private {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}
