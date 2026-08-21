package controllers

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

type InstanceRestartReason string

const (
	InstanceRestartReasonManual             InstanceRestartReason = "manual"
	InstanceRestartReasonHealthCheckFailure InstanceRestartReason = "health-check-failure"
	InstanceRestartReasonUpdate             InstanceRestartReason = "update"
	InstanceRestartReasonFailure            InstanceRestartReason = "failure"
)

// InstanceController owns instance lifecycle: create/retry/recreate/
// update/stop/delete/restart, endpoint publishing, attach/read
// operations, and classification. Consumers depend on the role
// interfaces they declare (reconcilerInstanceOps, healthInstanceOps,
// serviceInstanceOps, the orchestrator facade's instanceOps), all
// satisfied by this one type — RUNE-311 Phase 3.
type InstanceController struct {
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
func (c *InstanceController) endpointBinding() (EndpointPublisher, string) {
	if b := c.epBinding.Load(); b != nil {
		return b.publisher, b.nodeID
	}
	return nil, ""
}

// resolver returns the current MountResolver, nil when none is wired.
// Read it at each use — never capture it — so runed's live swap of the
// not-ready stub for the real subsystem is observed.
func (c *InstanceController) resolver() MountResolver {
	if b := c.mountRes.Load(); b != nil {
		return b.r
	}
	return nil
}

// MountResolver is the orchestrator-side surface of the agent's
// volumes Subsystem. The runed agent supplies an implementation
// backed by internal/agent/volumes.Subsystem.
type MountResolver interface {
	// MountTargetFor returns the host-side mount target for the named
	// volume on this node, plus a presence flag. A false return means
	// the subsystem has not (yet) mounted the volume locally; callers
	// should treat this as a transient "not ready" condition.
	MountTargetFor(volumeID string) (string, bool)
}

// MountErrorReporter is an optional companion to MountResolver that explains
// WHY a volume is not mounted. Without it, an instance blocked on storage only
// ever reported "not yet mounted (will retry)", which reads as a benign
// transient. In practice an expired cloud credential once stranded volumes
// that were attached and mounted the whole time, with the real HTTP 401
// visible only in the agent's startup log — undiagnosable from the CLI.
// Implementations return the most recent bring-up failure for the volume.
type MountErrorReporter interface {
	MountErrorFor(volumeID string) (string, bool)
}

// EndpointPublisher is the orchestrator-side surface of the
// networking data plane. The runed agent supplies an implementation
// backed by pkg/networking/endpoints + pkg/networking/localinstances.
type EndpointPublisher interface {
	// PublishService re-publishes the full Endpoint set for a service.
	// `endpoints` may be empty (the service has no running instances)
	// in which case the publisher should clear/Delete.
	PublishService(ctx context.Context, service *types.Service, endpoints []types.Endpoint) error
	// PublishLocalInstances re-publishes the per-node identity table
	// for `nodeID` so that policy enforcement can map source IPs back
	// to a service identity.
	PublishLocalInstances(ctx context.Context, nodeID string, table map[string]types.InstanceIdentity) error
}

// InstanceControllerOption configures NewInstanceController.
type InstanceControllerOption func(*InstanceController)

// WithEventLog wires the persisted event log (RUNE-126 Phase 2) at
// construction. Nil is accepted and keeps emit a no-op.
func WithEventLog(eventLog events.EventLog) InstanceControllerOption {
	return func(c *InstanceController) { c.events = eventLog }
}

// NewInstanceController creates a new instance controller. Construction
// covers every dependency that exists at startup; the endpoint publisher
// and mount resolver are deliberately NOT options — they depend on agent
// identity that runed only has after start, and arrive late through
// SetEndpointPublisher / SetMountResolver.
func NewInstanceController(store store.Store, runnerManager manager.IRunnerManager, logger log.Logger, opts ...InstanceControllerOption) *InstanceController {
	secretRepo := repos.NewSecretRepo(store)
	configRepo := repos.NewConfigRepo(store)
	c := &InstanceController{
		store:         store,
		runnerManager: runnerManager,
		logger:        logger.WithComponent("instance-controller"),
		env:           newEnvResolver(secretRepo, configRepo),
		lastPublished: map[string]string{},
	}
	// The binder reads mountResolver and nodeID through the controller at
	// each use — never captured — because runed wires both into a live
	// controller after start (RUNE-311 D4).
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
func (c *InstanceController) SetEndpointPublisher(publisher EndpointPublisher, nodeID string) {
	c.epBinding.Store(&endpointBinding{publisher: publisher, nodeID: nodeID})
}

// SetMountResolver wires the agent-side volumes Subsystem (RUNE-069)
// into the controller. LATE-BOUND: runed seeds a not-ready stub at
// construction and swaps the real subsystem in on a live controller
// after start. Passing nil disables the resolver-first path;
// resolveVolumeMount then uses Volume.Handle exclusively (the previous
// behaviour, correct only for in-tree local / local-host drivers).
func (c *InstanceController) SetMountResolver(resolver MountResolver) {
	c.mountRes.Store(&mountResolverBox{r: resolver})
}

// emit records one event for the instance. Fire-and-forget — emission
// failures are logged but never surfaced to the caller (events are
// observability, never on a correctness path).
func (c *InstanceController) emit(level types.EventLevel, instance *types.Instance, reason, message string) {
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

// Compile-time proof the controller satisfies every consumer's role
// interface.
var (
	_ reconcilerInstanceOps = (*InstanceController)(nil)
	_ serviceInstanceOps    = (*InstanceController)(nil)
	_ healthInstanceOps     = (*InstanceController)(nil)
)
