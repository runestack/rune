package controllers

import (
	"context"
	"io"
	"net"
	"sync"
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

// InstanceController manages instance lifecycle
type InstanceController interface {
	// GetInstanceByID gets an instance by ID
	GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error)

	// ListInstances lists all instances in a namespace
	ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error)

	// GetRunningInstances lists all running instances
	ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error)

	// CreateInstance creates a new instance for a service. ordinal is the
	// per-replica slot index assigned by the reconciler; it is stored on the
	// instance and drives per-replica volume claimTemplate binding.
	CreateInstance(ctx context.Context, service *types.Service, instanceName string, ordinal int) (*types.Instance, error)

	// RetryCreateInstance re-runs the create pipeline against an
	// existing record that previously failed in a stuck-in-create
	// state (Status=Failed, ContainerEverCreatedAt==nil). Same UUID,
	// same Name. Called by the reconciler when the stuck record's
	// NextCreateAttemptAt backoff has elapsed.
	RetryCreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error

	// RecreateInstance recreates an instance
	RecreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) (*types.Instance, error)

	// UpdateInstance updates an existing instance
	UpdateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error

	// StopInstance stops an instance temporarily but keeps it in the store
	StopInstance(ctx context.Context, instance *types.Instance) error

	// WithdrawServiceInstances removes a set of instances from the dataplane
	// endpoint set in one publish and takes ONE shared drain window for all
	// of them (RUNE-042 §4: whole-service teardowns drain in batch, not in
	// series — a per-instance drain would add len(instances) × drainWindow
	// to a teardown whose instances are all being withdrawn anyway). Each
	// Running instance is flipped to Terminating first, so the per-instance
	// teardown that follows (StopInstance/DeleteInstance) sees a
	// non-Running status and skips its own drain. Best-effort; never fails
	// the teardown.
	WithdrawServiceInstances(ctx context.Context, service *types.Service, instances []*types.Instance)

	// DeleteInstance marks an instance for deletion and cleans up runner resources
	// The instance will remain in the store with Deleted status until garbage collection
	DeleteInstance(ctx context.Context, instance *types.Instance) error

	// GetInstanceStatus gets the current status of an instance
	GetInstanceStatus(ctx context.Context, instance *types.Instance) (*types.InstanceStatusInfo, error)

	// GetInstanceLogs gets logs for an instance
	GetInstanceLogs(ctx context.Context, instance *types.Instance, opts types.LogOptions) (io.ReadCloser, error)

	// RestartInstance restarts an instance with respect to the service's restart policy
	RestartInstance(ctx context.Context, instance *types.Instance, reason InstanceRestartReason) error

	// Exec executes a command in a running instance
	// Returns an ExecStream for bidirectional communication
	Exec(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error)

	// ExecDebug spawns an ephemeral inspection sidecar from a Failed
	// instance's template (image, env, mounts), with the entrypoint
	// overridden to `sleep infinity`, and execs options.Command inside
	// the sidecar. The sidecar is removed when the returned ExecStream
	// is Closed. The original Failed container is never touched.
	ExecDebug(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error)

	// Dial opens a TCP connection to the given port on a running
	// instance (RUNE-122). Returns a net.Conn owned by the caller.
	Dial(ctx context.Context, instance *types.Instance, port uint32) (net.Conn, error)

	// SetEndpointPublisher wires the networking data plane (RUNE-063).
	// May be called once at startup; nil-publisher disables publishing.
	SetEndpointPublisher(publisher EndpointPublisher, nodeID string)

	// SetMountResolver wires the agent-side volumes Subsystem
	// (RUNE-069). May be called once at startup; nil disables
	// resolver-first mount-target lookup and falls back to
	// Volume.Handle.
	SetMountResolver(resolver MountResolver)

	// SetEventLog wires the persisted event log (RUNE-126 Phase 2)
	// so status transitions surface in `rune describe`. Nil-safe.
	SetEventLog(eventLog events.EventLog)

	// collectRunningInstances gathers all running instances from all runners
	collectRunningInstances(ctx context.Context) (map[string]*RunningInstance, error)

	// isInstanceCompatibleWithService checks if an instance is compatible with a service
	isInstanceCompatibleWithService(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string)

	// classifyInstance is the richer view isInstanceCompatibleWithService
	// wraps: it distinguishes a BROKEN instance (repair now, unbudgeted)
	// from an OUTDATED one (serving, replacement governed by the update
	// budget). See CompatClass.
	classifyInstance(ctx context.Context, instance *types.Instance, service *types.Service) CompatVerdict

	// RepublishServiceByInstance recomputes the data-plane endpoint set
	// for the service owning the given instance. Exposed for callers
	// outside instanceController that change instance reachability
	// (e.g. the health controller promoting Starting→Running on the
	// first readiness pass). Nil-safe when no endpoint publisher is
	// wired; safe to call repeatedly.
	RepublishServiceByInstance(ctx context.Context, instance *types.Instance)

	// RepublishService refreshes the dataplane endpoint set from
	// current store state (including live container IPs). Safe to
	// call on every reconcile tick.
	RepublishService(ctx context.Context, service *types.Service)
}

// instanceController implements the InstanceController interface
type instanceController struct {
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
	// Set after construction via SetEventLog; nil-safe (emit is a
	// no-op) so unit tests don't need to wire one.
	events events.EventLog

	// endpointPublisher and nodeID power the RUNE-063 networking
	// data plane. When non-nil, every successful instance lifecycle
	// transition (Create/Update/Stop/Delete) re-derives the full
	// endpoint set for the affected service from the store and pushes
	// it through OrderedLog so the agent's load-balancer + DNS see a
	// single, ordered view of reality. Nil-safe: in dev/standalone
	// mode the controller leaves networking to the runner.
	endpointPublisher EndpointPublisher
	nodeID            string

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

	// mountResolver, when non-nil, lets resolveVolumeMount consult
	// the agent-side volumes Subsystem (RUNE-069 Slice 4) for the
	// per-node mount target before falling back to Volume.Handle.
	// The fallback preserves correctness for the in-tree local /
	// local-host drivers (their Mount returns the host path verbatim,
	// so target == handle); the resolver-first path is what makes
	// future block-device drivers (do-volume, ...) usable, since for
	// those Volume.Handle is a cloud-side identifier rather than a
	// host filesystem path. Nil-safe.
	mountResolver MountResolver
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

// NewInstanceController creates a new instance controller
func NewInstanceController(store store.Store, runnerManager manager.IRunnerManager, logger log.Logger) InstanceController {
	secretRepo := repos.NewSecretRepo(store)
	configRepo := repos.NewConfigRepo(store)
	c := &instanceController{
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
		func() MountResolver { return c.mountResolver },
		func() string { return c.nodeID })
	return c
}

// SetEndpointPublisher wires the networking data plane (RUNE-063)
// into the controller. Call once at startup from runed before any
// reconciles are processed. nodeID identifies the host running this
// controller and is used as the LocalInstances table key. Passing a
// nil publisher disables endpoint publication (dev/standalone mode).
func (c *instanceController) SetEndpointPublisher(publisher EndpointPublisher, nodeID string) {
	c.endpointPublisher = publisher
	c.nodeID = nodeID
}

// SetMountResolver wires the agent-side volumes Subsystem (RUNE-069)
// into the controller. Call once at startup from runed before any
// reconciles are processed. Passing nil disables the resolver-first
// path; resolveVolumeMount then uses Volume.Handle exclusively
// (the previous behaviour, correct only for in-tree local /
// local-host drivers).
func (c *instanceController) SetMountResolver(resolver MountResolver) {
	c.mountResolver = resolver
}

// SetEventLog wires the persisted event log (RUNE-126 Phase 2). Nil
// is accepted and turns emit into a no-op so unit tests and callers
// that don't want events keep working unchanged.
func (c *instanceController) SetEventLog(eventLog events.EventLog) {
	c.events = eventLog
}

// emit records one event for the instance. Fire-and-forget — emission
// failures are logged but never surfaced to the caller (events are
// observability, never on a correctness path).
func (c *instanceController) emit(level types.EventLevel, instance *types.Instance, reason, message string) {
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
