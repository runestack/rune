package instance

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/wiring"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

type RestartReason string

const (
	RestartReasonManual             RestartReason = "manual"
	RestartReasonHealthCheckFailure RestartReason = "health-check-failure"
	RestartReasonUpdate             RestartReason = "update"
	RestartReasonFailure            RestartReason = "failure"
)

// Controller owns instance lifecycle: create/retry/recreate/
// update/stop/delete/restart, endpoint publishing, attach/read
// operations, and classification. Consumers depend on the role
// interfaces they declare (reconcilerInstanceOps, healthInstanceOps,
// serviceInstanceOps, the orchestrator facade's instanceOps), all
// satisfied by this one type — RUNE-311 Phase 3.
type Controller struct {
	store         store.Store
	runnerManager manager.IRunnerManager
	logger        log.Logger

	// env and mounts are the RUNE-311 Phase 2 collaborators: env owns
	// environment preparation + {{...}} interpolation, mounts owns
	// secret/config/volume mount resolution. The controller keeps thin
	// same-named methods delegating to them.
	env    *envResolver
	mounts *mountBinder

	// events is the optional persisted event log (RUNE-126 Phase 2).
	// Wired at construction via WithEventLog; nil-safe (emit is a
	// no-op) so unit tests don't need to wire one.
	events events.EventLog

	// epBinding powers the RUNE-063 networking data plane: the
	// publisher and the node identity it was wired with, behind ONE
	// atomic pointer because the pair is read together and nodeID is
	// also read by the mount path. Late-bound BY DESIGN (RUNE-311 D4):
	// runed calls SetEndpointPublisher on a live controller after
	// agent identity exists, while reconcile goroutines read — a plain
	// field write here was a data race. When unset, every lifecycle
	// transition leaves networking to the runner (dev/standalone).
	epBinding atomic.Pointer[endpointBinding]

	// drainWindowOverride, when non-zero, replaces the per-service drain
	// window. Test-only seam so ordering assertions don't have to wait out a
	// whole second — real deployments derive the window from the service's
	// drainSeconds (RUNE-042 §4/§5.1) so operators can tune it per service.
	drainWindowOverride time.Duration

	// publishedMu guards lastPublished, a service-ID -> last-published
	// endpoint-set signature map. republishService is now called every
	// reconcile tick; without this dedup every service would append a
	// no-op endpoint mutation to the OrderedLog every tick, growing the
	// log indefinitely for a cluster that isn't changing.
	publishedMu   sync.Mutex
	lastPublished map[string]string

	// nodeID is the identity of the machine this controller places
	// instances on — agent.Identity().NodeID, the SAME string
	// Volume.BoundNode and the observability stream label already use.
	// Wired at construction because runed reads node-identity.json before
	// the reconciler's first pass. Empty only where no identity exists at
	// all (tests, embedded use); CreateInstance then falls back to
	// types.LocalNodeIDFallback.
	nodeID string

	// mountRes, when set, lets resolveVolumeMount consult the
	// agent-side volumes Subsystem (RUNE-069 Slice 4) for the per-node
	// mount target before falling back to Volume.Handle. The fallback
	// preserves correctness for the in-tree local / local-host drivers
	// (their Mount returns the host path verbatim, so target ==
	// handle); the resolver-first path is what makes future
	// block-device drivers (do-volume, ...) usable, since for those
	// Volume.Handle is a cloud-side identifier rather than a host
	// filesystem path. Same late-bound atomic story as epBinding:
	// runed seeds a not-ready stub at construction and swaps the real
	// subsystem in after start. Nil-safe when never set.
	mountRes atomic.Pointer[mountResolverBox]
}

// endpointBinding is the atomically-swapped {publisher, nodeID} pair —
// one allocation so readers always see a consistent pairing.
type endpointBinding struct {
	publisher EndpointPublisher
	nodeID    string
}

// mountResolverBox boxes the MountResolver interface value so it can sit
// behind an atomic.Pointer.
type mountResolverBox struct {
	r MountResolver
}

// endpointBinding returns the current publisher and node identity as a
// consistent pair; (nil, "") when networking is not wired.
func (c *Controller) endpointBinding() (EndpointPublisher, string) {
	if b := c.epBinding.Load(); b != nil {
		return b.publisher, b.nodeID
	}
	return nil, ""
}

// resolver returns the current MountResolver, nil when none is wired.
// Read it at each use — never capture it — so runed's live swap of the
// not-ready stub for the real subsystem is observed.
func (c *Controller) resolver() MountResolver {
	if b := c.mountRes.Load(); b != nil {
		return b.r
	}
	return nil
}

// The agent-implemented seams live in pkg/orchestrator/wiring
// (RUNE-311 Phase 4); aliased here so the controller's own code and
// signatures read naturally.
type (
	MountResolver      = wiring.MountResolver
	MountErrorReporter = wiring.MountErrorReporter
	EndpointPublisher  = wiring.EndpointPublisher
)

// Option configures NewController.
type Option func(*Controller)

// WithEventLog wires the persisted event log (RUNE-126 Phase 2) at
// construction. Nil is accepted and keeps emit a no-op.
func WithEventLog(eventLog events.EventLog) Option {
	return func(c *Controller) { c.events = eventLog }
}

// WithNodeID wires this node's identity. Empty is accepted and leaves
// CreateInstance on types.LocalNodeIDFallback.
func WithNodeID(nodeID string) Option {
	return func(c *Controller) { c.nodeID = nodeID }
}

// NodeID returns the identity of the node this controller places
// instances on, or types.LocalNodeIDFallback when none was wired.
func (c *Controller) NodeID() string {
	if c.nodeID != "" {
		return c.nodeID
	}
	return types.LocalNodeIDFallback
}

// NewController creates a new instance controller. Construction
// covers every dependency that exists at startup; the endpoint publisher
// and mount resolver are deliberately NOT options — they depend on agent
// identity that runed only has after start, and arrive late through
// SetEndpointPublisher / SetMountResolver.
func NewController(store store.Store, runnerManager manager.IRunnerManager, logger log.Logger, opts ...Option) *Controller {
	secretRepo := repos.NewSecretRepo(store)
	configRepo := repos.NewConfigRepo(store)
	c := &Controller{
		store:         store,
		runnerManager: runnerManager,
		logger:        logger.WithComponent("instance-controller"),
		env:           newEnvResolver(secretRepo, configRepo),
		lastPublished: map[string]string{},
	}
	// The binder reads mountResolver and nodeID through the controller at
	// each use — never captured — because runed wires both into a live
	// controller after start.
	//
	// Its node ID comes from endpointBinding, NOT from c.NodeID(), and the
	// difference is load-bearing: endpointBinding is empty until the
	// networking data plane is wired and resolveVolumeMount skips binding
	// while it is, whereas c.NodeID() is never empty (it falls back to a
	// literal). Switching this to c.NodeID() would start binding volumes
	// earlier AND would make embedded callers with no identity bind every
	// volume to that fallback literal, which is not a node.
	c.mounts = newMountBinder(store, secretRepo, configRepo,
		c.resolver,
		func() string { _, nodeID := c.endpointBinding(); return nodeID })
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SetEndpointPublisher wires the networking data plane (RUNE-063) into
// the controller. LATE-BOUND: runed calls this on a live controller
// after agent identity exists, while reconciles are already running —
// the atomic swap is what makes that safe. nodeID identifies the host
// running this controller and is used as the LocalInstances table key.
// Passing a nil publisher disables endpoint publication
// (dev/standalone mode).
func (c *Controller) SetEndpointPublisher(publisher EndpointPublisher, nodeID string) {
	c.epBinding.Store(&endpointBinding{publisher: publisher, nodeID: nodeID})
}

// SetMountResolver wires the agent-side volumes Subsystem (RUNE-069)
// into the controller. LATE-BOUND: runed seeds a not-ready stub at
// construction and swaps the real subsystem in on a live controller
// after start. Passing nil disables the resolver-first path;
// resolveVolumeMount then uses Volume.Handle exclusively (the previous
// behaviour, correct only for in-tree local / local-host drivers).
func (c *Controller) SetMountResolver(resolver MountResolver) {
	c.mountRes.Store(&mountResolverBox{r: resolver})
}

// emit records one event for the instance. Fire-and-forget — emission
// failures are logged but never surfaced to the caller (events are
// observability, never on a correctness path).
func (c *Controller) emit(level types.EventLevel, instance *types.Instance, reason, message string) {
	if c.events == nil || instance == nil {
		return
	}
	if err := c.events.Emit(context.Background(), types.Event{
		Namespace: instance.Namespace,
		Kind:      "Instance",
		Name:      instance.Name,
		UID:       instance.ID,
		Level:     level,
		Reason:    reason,
		Message:   message,
	}); err != nil {
		c.logger.Warn("Failed to emit instance event",
			log.Str("instance", instance.Name), log.Err(err))
	}
}

// applyInstanceStatus sets the instance's lifecycle status, its
// machine-readable reason slug and its human-readable message in one
// place, advancing LastTransitionAt only when the Status value
// actually changes. Every reconciler status write should go through
// here so `rune describe` always sees a populated reason and an
// accurate "<status> for <duration>". Pass reason="" for Running.
func applyInstanceStatus(instance *types.Instance, status types.InstanceStatus, reason, message string) {
	now := time.Now()
	if instance.Status != status || instance.LastTransitionAt == nil {
		instance.LastTransitionAt = &now
	}
	instance.Status = status
	instance.StatusReason = reason
	instance.StatusMessage = message
	instance.UpdatedAt = now
}
