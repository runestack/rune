package controllers

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
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

// execStreamAdapter adapts runner.ExecStream to orchestrator.ExecStream
type execStreamAdapter struct {
	runner.ExecStream
}

// InstanceController manages instance lifecycle
type InstanceController interface {
	// GetInstanceByID gets an instance by ID
	GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error)

	// ListInstances lists all instances in a namespace
	ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error)

	// GetRunningInstances lists all running instances
	ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error)

	// CreateInstance creates a new instance for a service
	CreateInstance(ctx context.Context, service *types.Service, instanceName string) (*types.Instance, error)

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

	// collectRunningInstances gathers all running instances from all runners
	collectRunningInstances(ctx context.Context) (map[string]*RunningInstance, error)

	// isInstanceCompatibleWithService checks if an instance is compatible with a service
	isInstanceCompatibleWithService(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string)
}

// instanceController implements the InstanceController interface
type instanceController struct {
	store         store.Store
	runnerManager manager.IRunnerManager
	logger        log.Logger
	secretRepo    *repos.SecretRepo
	configRepo    *repos.ConfigmapRepo

	// endpointPublisher and nodeID power the RUNE-063 networking
	// data plane. When non-nil, every successful instance lifecycle
	// transition (Create/Update/Stop/Delete) re-derives the full
	// endpoint set for the affected service from the store and pushes
	// it through OrderedLog so the agent's load-balancer + DNS see a
	// single, ordered view of reality. Nil-safe: in dev/standalone
	// mode the controller leaves networking to the runner.
	endpointPublisher EndpointPublisher
	nodeID            string

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
	return &instanceController{
		store:         store,
		runnerManager: runnerManager,
		logger:        logger.WithComponent("instance-controller"),
		secretRepo:    repos.NewSecretRepo(store),
		configRepo:    repos.NewConfigRepo(store),
	}
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

// republishService recomputes the endpoint set for a service from
// the current store contents and publishes it. Best-effort: errors
// are logged but never surfaced because a failure to publish must
// not roll back the runner-side lifecycle transition that already
// succeeded.
func (c *instanceController) republishService(ctx context.Context, service *types.Service) {
	if c.endpointPublisher == nil || service == nil {
		return
	}
	var instances []*types.Instance
	if err := c.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances); err != nil {
		c.logger.Warn("republishService: list instances failed",
			log.Str("service", service.Name),
			log.Err(err))
		return
	}
	primaryPort := 0
	primaryProto := "TCP"
	if len(service.Ports) > 0 {
		primaryPort = service.Ports[0].Port
		if service.Ports[0].TargetPort != 0 {
			primaryPort = service.Ports[0].TargetPort
		}
		if service.Ports[0].Protocol != "" {
			primaryProto = service.Ports[0].Protocol
		}
	}
	eps := make([]types.Endpoint, 0)
	for _, inst := range instances {
		if inst == nil || inst.ServiceName != service.Name {
			continue
		}
		if inst.Status != types.InstanceStatusRunning {
			continue
		}
		ip := ""
		if inst.Metadata != nil {
			ip = inst.Metadata.ContainerIP
		}
		if ip == "" {
			continue
		}
		md := map[string]string{}
		if c.nodeID != "" {
			md["node_id"] = c.nodeID
		}
		eps = append(eps, types.Endpoint{
			InstanceID: inst.ID,
			IP:         ip,
			Port:       primaryPort,
			Protocol:   primaryProto,
			Metadata:   md,
			Healthy:    true,
		})
	}
	if err := c.endpointPublisher.PublishService(ctx, service, eps); err != nil {
		c.logger.Warn("republishService: publish failed",
			log.Str("service", service.Name),
			log.Err(err))
	}
}

// republishServiceByInstance is a convenience wrapper that loads the
// owning service from the store before delegating to republishService.
// Used by lifecycle methods (Stop/Delete) that hold an Instance but
// not its Service.
func (c *instanceController) republishServiceByInstance(ctx context.Context, instance *types.Instance) {
	if c.endpointPublisher == nil || instance == nil || instance.ServiceName == "" {
		return
	}
	var svc types.Service
	if err := c.store.Get(ctx, types.ResourceTypeService, instance.Namespace, instance.ServiceName, &svc); err != nil {
		c.logger.Debug("republishServiceByInstance: service lookup failed",
			log.Str("service", instance.ServiceName),
			log.Err(err))
		return
	}
	c.republishService(ctx, &svc)
}

// republishLocalInstances rebuilds the per-node InstanceIdentity
// table from current store state across all namespaces and pushes
// it. Best-effort.
func (c *instanceController) republishLocalInstances(ctx context.Context) {
	if c.endpointPublisher == nil || c.nodeID == "" {
		return
	}
	running, err := c.collectRunningInstances(ctx)
	if err != nil {
		c.logger.Warn("republishLocalInstances: collectRunning failed", log.Err(err))
		return
	}
	table := make(map[string]types.InstanceIdentity, len(running))
	for id, ri := range running {
		if ri == nil || ri.Instance == nil {
			continue
		}
		ip := ""
		if ri.Instance.Metadata != nil {
			ip = ri.Instance.Metadata.ContainerIP
		}
		if ip == "" {
			continue
		}
		table[ip] = types.InstanceIdentity{
			InstanceID: id,
			Service:    ri.Instance.ServiceName,
			Namespace:  ri.Instance.Namespace,
		}
	}
	if err := c.endpointPublisher.PublishLocalInstances(ctx, c.nodeID, table); err != nil {
		c.logger.Warn("republishLocalInstances: publish failed", log.Err(err))
	}
}

func (c *instanceController) GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	return c.store.GetInstanceByID(ctx, namespace, instanceID)
}

func (c *instanceController) ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	var instances []*types.Instance
	err := c.store.List(ctx, types.ResourceTypeInstance, namespace, &instances)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	return instances, nil
}

func (c *instanceController) ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	runningInstances, err := c.collectRunningInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list running instances: %w", err)
	}

	// get all instances from store
	var storeInstances []types.Instance
	err = c.store.List(ctx, types.ResourceTypeInstance, "", &storeInstances)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	// filter instances by running instances
	runningInstancesPointers := make([]*types.Instance, 0, len(runningInstances))
	for _, instance := range runningInstances {
		for _, storeInstance := range storeInstances {
			if instance.Instance.ID == storeInstance.ID && storeInstance.Namespace == namespace {
				runningInstancesPointers = append(runningInstancesPointers, &storeInstance)
			}
		}
	}

	return runningInstancesPointers, nil
}

// CreateInstance creates a new instance for a service
// This would be simplified to only handle the pure creation case
func (c *instanceController) CreateInstance(ctx context.Context, service *types.Service, instanceName string) (*types.Instance, error) {
	c.logger.Info("Creating new instance",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Str("instance", instanceName))

	// Create instance object
	instance := &types.Instance{
		ID:          uuid.New().String(),
		Name:        instanceName,
		Namespace:   service.Namespace,
		ServiceName: service.Name,
		ServiceID:   service.ID,
		NodeID:      "local",
		Status:      types.InstanceStatusPending,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Metadata:    &types.InstanceMetadata{},
	}

	// Propagate resolved resource constraints from service to instance
	// Use a pointer so runners can access limits/requests directly
	instance.Resources = &service.Resources

	// Propagate SecurityContext from service to instance so the runner
	// can apply seccomp / capabilities / privileged to the main
	// container. Init steps carry their own SecurityContext.
	instance.SecurityContext = service.SecurityContext

	// Store the service generation in instance metadata. ServiceMetadata
	// may be nil for hand-built services in tests; treat as generation 0.
	if service.Metadata != nil {
		instance.Metadata.ServiceGeneration = service.Metadata.Generation
	}

	// Propagate ports and expose spec for runner use and later status
	if len(service.Ports) > 0 {
		instance.Metadata.Ports = append(instance.Metadata.Ports, service.Ports...)
	}
	if service.Expose != nil {
		instance.Metadata.Expose = service.Expose
	}
	if service.Metadata != nil {
		c.logger.Debug("Storing service generation in instance",
			log.Str("instance", instanceName),
			log.Int64("generation", service.Metadata.Generation))
	}

	// Save instance to store
	if err := c.store.Create(ctx, types.ResourceTypeInstance, service.Namespace, instance.ID, instance); err != nil {
		return nil, fmt.Errorf("failed to create instance in store: %w", err)
	}

	if err := c.runCreateAttempt(ctx, service, instance); err != nil {
		return nil, err
	}
	return instance, nil
}

// RetryCreateInstance re-runs the CreateInstance pipeline against an
// existing instance record (same UUID, same Name) that previously
// failed in a stuck-in-create state (Status=Failed,
// ContainerEverCreatedAt==nil). Called by the reconciler when a
// stuck record's NextCreateAttemptAt backoff has elapsed.
//
// Resets transient state (Status→Pending, StatusMessage cleared,
// NextCreateAttemptAt cleared) before re-running the create pipeline.
// CreateAttempts is preserved so the backoff schedule and Stalled
// threshold see the cumulative history.
func (c *instanceController) RetryCreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	if instance == nil {
		return fmt.Errorf("retry: nil instance")
	}
	c.logger.Info("Retrying create on stuck-in-create instance",
		log.Str("service", service.Name),
		log.Str("instance", instance.Name),
		log.Str("id", instance.ID),
		log.Int("attempt", instance.CreateAttempts+1))

	now := time.Now()
	instance.Status = types.InstanceStatusPending
	instance.StatusMessage = ""
	instance.NextCreateAttemptAt = nil
	instance.UpdatedAt = now
	if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
		return fmt.Errorf("reset instance for retry: %w", err)
	}

	return c.runCreateAttempt(ctx, service, instance)
}

// runCreateAttempt is the shared body of CreateInstance and
// RetryCreateInstance — everything after the instance record exists
// in the store. Walks the create pipeline (runner lookup → env →
// mounts → init steps → runner.Create → ContainerEverCreatedAt
// stamp → runner.Start → Running). Every error path routes through
// recordCreateFailure so the reason lands on the record and backoff
// is scheduled.
func (c *instanceController) runCreateAttempt(ctx context.Context, service *types.Service, instance *types.Instance) error {
	// Create instance based on runtime
	serviceRunner, err := c.runnerManager.GetServiceRunner(service)
	if err != nil {
		wrapped := fmt.Errorf("failed to get runner for service: %w", err)
		c.recordCreateFailure(ctx, instance, wrapped, classifyCreateError(wrapped))
		return wrapped
	}

	// Set the runner type for the instance
	instance.Runner = serviceRunner.Type()

	// Build environment variables and interpolate secret:/config: values
	envVars, err := c.prepareEnvVars(ctx, service, instance)
	if err != nil {
		wrapped := fmt.Errorf("failed to prepare environment variables: %w", err)
		c.recordCreateFailure(ctx, instance, wrapped, classifyCreateError(wrapped))
		return wrapped
	}
	c.logger.Debug("Prepared environment variables",
		log.Str("instance", instance.Name),
		log.Int("env_var_count", len(envVars)))

	// Set environment variables in the instance
	instance.Environment = envVars

	// Resolve secret and config mounts for this instance
	if err := c.resolveMounts(ctx, service, instance); err != nil {
		wrapped := fmt.Errorf("failed to resolve secret and config mounts: %w", err)
		c.recordCreateFailure(ctx, instance, wrapped, classifyCreateError(wrapped))
		return wrapped
	}

	// Store original image for future compatibility checks
	if service.Image != "" {
		if instance.Metadata == nil {
			instance.Metadata = &types.InstanceMetadata{}
		}
		instance.Metadata.Image = service.Image
		instance.Metadata.ImagePull = service.ImagePull
	}

	// Propagate the main container's command/args so the runner can
	// override the image's ENTRYPOINT/CMD (Kubernetes semantics:
	// command → Entrypoint, args → Cmd). Empty values leave the
	// image's baked-in defaults in place. See pkg/runner/docker/runner.go.
	if instance.Metadata == nil {
		instance.Metadata = &types.InstanceMetadata{}
	}
	instance.Metadata.Command = service.Command
	if len(service.Args) > 0 {
		instance.Metadata.Args = append([]string(nil), service.Args...)
	}

	// Run init steps before the main container is created (RUNE-121).
	// On failure this sets instance.Status=Failed and returns an error.
	if err := c.runInitSteps(ctx, serviceRunner, service, instance); err != nil {
		wrapped := fmt.Errorf("init steps failed: %w", err)
		c.recordCreateFailure(ctx, instance, wrapped, classifyCreateError(wrapped))
		return wrapped
	}

	// Update instance with pending status
	instance.Status = types.InstanceStatusStarting
	if err := c.store.Update(ctx, types.ResourceTypeInstance, service.Namespace, instance.ID, instance); err != nil {
		c.logger.Error("Failed to update instance status",
			log.Str("instance", instance.ID),
			log.Err(err))
	}

	// Create the instance using the runner
	if err := serviceRunner.Create(ctx, instance); err != nil {
		wrapped := fmt.Errorf("failed to create instance: %w", err)
		c.recordCreateFailure(ctx, instance, wrapped, classifyCreateError(wrapped))
		return wrapped
	}

	// Stamp the first-success marker so the reconciler can tell apart
	// "container vanished" (recreate is correct) from "create never
	// succeeded" (recreate would just churn — keep retrying the same
	// record). Set once; never cleared.
	if instance.ContainerEverCreatedAt == nil {
		now := time.Now()
		instance.ContainerEverCreatedAt = &now
	}

	// Start the instance
	if err := serviceRunner.Start(ctx, instance); err != nil {
		wrapped := fmt.Errorf("failed to start instance: %w", err)
		c.recordCreateFailure(ctx, instance, wrapped, classifyCreateError(wrapped))
		return wrapped
	}

	// Update instance with running status. Clear all retry bookkeeping
	// so the record looks pristine — operators querying a healthy
	// instance shouldn't see leftover attempt counts from earlier
	// stuck-in-create retries.
	instance.Status = types.InstanceStatusRunning
	instance.StatusMessage = "Created successfully"
	instance.CreateAttempts = 0
	instance.NextCreateAttemptAt = nil
	if err := c.store.Update(ctx, types.ResourceTypeInstance, service.Namespace, instance.ID, instance); err != nil {
		c.logger.Error("Failed to update instance status",
			log.Str("instance", instance.ID),
			log.Err(err))
	}

	// Networking data plane (RUNE-063): republish service endpoints +
	// per-node identity table now that this instance is Running and
	// has a ContainerIP recorded by the runner.
	c.republishService(ctx, service)
	c.republishLocalInstances(ctx)

	return nil
}

// RecreateInstance destroys an existing instance and creates a new one with the same name
func (c *instanceController) RecreateInstance(ctx context.Context, service *types.Service, existingInstance *types.Instance) (*types.Instance, error) {
	instanceName := existingInstance.Name
	c.logger.Info("Recreating instance",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Str("instance", instanceName))

	// Delete the existing instance
	if err := c.DeleteInstance(ctx, existingInstance); err != nil {
		return nil, fmt.Errorf("failed to delete instance for recreation: %w", err)
	}

	// Create a new instance
	return c.CreateInstance(ctx, service, instanceName)
}

// UpdateInstance updates an existing instance
func (c *instanceController) UpdateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	c.logger.Debug("Checking instance for updates",
		log.Str("instance", instance.ID),
		log.Str("service", service.Name))

	// Get current runner for this instance
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return fmt.Errorf("failed to get runner for instance: %w", err)
	}

	// Check if the instance is compatible with the current service definition
	isCompatible, reason := c.isInstanceCompatibleWithService(ctx, instance, service)
	if !isCompatible {
		c.logger.Info("Instance is not compatible with current service definition, recreation required",
			log.Str("instance", instance.ID),
			log.Str("service", service.Name),
			log.Str("reason", reason))

		// For now, we'll stop and return an error indicating that recreation is needed
		// The caller should handle the recreation
		return fmt.Errorf("instance %s requires recreation to update: %s", instance.ID, reason)
	}

	// For compatible instances, we can apply in-place updates
	// First check if instance is running
	status, err := runner.Status(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get instance status: %w", err)
	}

	// Check if the instance is in a state that can be updated
	if status != types.InstanceStatusRunning {
		c.logger.Info("Instance is not in a state that can be updated in-place",
			log.Str("instance", instance.ID),
			log.Str("currentStatus", string(status)))
		return fmt.Errorf("instance %s is in state %s and cannot be updated in-place", instance.ID, status)
	}

	// Apply updates to the instance object
	instanceUpdated := false

	// Update environment variables (only adding/modifying, not removing)
	envVarsUpdated := false
	envVars, err := c.prepareEnvVars(ctx, service, instance)
	if err != nil {
		return fmt.Errorf("failed to prepare environment variables: %w", err)
	}
	for key, value := range envVars {
		// Skip internal RUNE environment variables for comparison
		if len(key) > 5 && key[:5] == "RUNE_" {
			continue
		}

		// Check if this is a new or changed env var
		currentValue, exists := instance.Environment[key]
		if !exists || currentValue != value {
			if instance.Environment == nil {
				instance.Environment = make(map[string]string)
			}
			instance.Environment[key] = value
			envVarsUpdated = true
		}
	}

	if envVarsUpdated {
		c.logger.Debug("Environment variables updated",
			log.Str("instance", instance.ID))
		instanceUpdated = true
	}

	// Update status message if needed
	if instance.StatusMessage == "" || instance.StatusMessage == "Created" {
		instance.StatusMessage = "Updated"
		instanceUpdated = true
	}

	// Update the stored service generation
	if instance.Metadata == nil {
		instance.Metadata = &types.InstanceMetadata{}
	}

	// Check if service generation has changed
	generationUpdated := instance.Metadata.ServiceGeneration != service.Metadata.Generation
	if generationUpdated {
		instance.Metadata.ServiceGeneration = service.Metadata.Generation
		instanceUpdated = true
		c.logger.Debug("Updating service generation in instance",
			log.Str("instance", instance.ID),
			log.Int64("generation", service.Metadata.Generation))
	}

	// Update timestamp only if we made meaningful changes
	if instanceUpdated {
		instance.UpdatedAt = time.Now()
	}

	// If we've made any updates to the instance object, save it back to the store
	if instanceUpdated {
		if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
			return fmt.Errorf("failed to update instance in store: %w", err)
		}
		c.logger.Info("Instance updated successfully",
			log.Str("instance", instance.ID),
			log.Str("service", service.Name))
	} else {
		c.logger.Debug("No changes needed for instance",
			log.Str("instance", instance.ID))
	}

	return nil
}

// StopInstance stops an instance but keeps it in the store
func (c *instanceController) StopInstance(ctx context.Context, instance *types.Instance) error {
	c.logger.Info("Stopping instance",
		log.Str("instance", instance.ID))

	// Get the runner for this instance
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return fmt.Errorf("failed to get runner for instance: %w", err)
	}

	// Stop the instance with the runner
	if err := runner.Stop(ctx, instance, 10*time.Second); err != nil {
		c.logger.Error("Failed to stop instance with runner",
			log.Str("instance", instance.ID),
			log.Err(err))
		return fmt.Errorf("failed to stop instance: %w", err)
	}

	// Update instance status to stopped
	originalStatus := instance.Status
	instance.Status = types.InstanceStatusStopped
	instance.UpdatedAt = time.Now()
	instance.StatusMessage = "Stopped by user"

	if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
		c.logger.Error("Failed to update instance status",
			log.Str("instance", instance.ID),
			log.Str("from", string(originalStatus)),
			log.Str("to", string(instance.Status)),
			log.Err(err))
		return fmt.Errorf("failed to update instance status: %w", err)
	}

	c.logger.Info("Instance stopped successfully",
		log.Str("instance", instance.ID))
	c.republishServiceByInstance(ctx, instance)
	c.republishLocalInstances(ctx)
	return nil
}

// markInstanceFailedInPlace transitions a failing instance to its terminal
// tombstone state — Status=Failed, FailedAt=now, FailureReason — while
// LEAVING ITS CONTAINER ALONE. The container has already been Stopped by
// the caller; we just don't remove it, so `rune logs <id>` and
// `rune exec --debug <id>` can still address it for postmortem.
//
// In the new naming scheme each container is named
// `<namespace>-<service>-<ordinal>-<id_prefix>` where id_prefix is derived
// from the instance UUID. That suffix means a freshly-created replacement
// instance NEVER collides with this preserved container on the docker
// side, so we don't need to rename or fork a separate "tombstone"
// instance record (the rename + new-record dance of the previous design
// is gone). The Failed instance record IS the tombstone.
//
// The reconciler filters out Failed+FailedAt records when looking for a
// live instance to occupy the service's name slot, so creating the
// replacement next reconcile tick (or synchronously from RestartInstance)
// will just work.
func (c *instanceController) markInstanceFailedInPlace(ctx context.Context, instance *types.Instance, restartReason InstanceRestartReason) error {
	now := time.Now()
	instance.Status = types.InstanceStatusFailed
	instance.StatusMessage = fmt.Sprintf("Preserved for postmortem after %s", restartReason)
	instance.FailedAt = &now
	instance.FailureReason = string(restartReason)
	instance.UpdatedAt = now
	if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
		return fmt.Errorf("mark instance failed: %w", err)
	}
	c.logger.Info("Marked instance Failed (tombstoned in-place)",
		log.Str("instance", instance.ID),
		log.Str("container_id", instance.ContainerID),
		log.Str("reason", string(restartReason)))
	return nil
}

// recordCreateFailure persists the failure of a CreateInstance attempt
// onto the existing instance record (same UUID, same Name) so operators
// can see why an instance is stuck via `rune get instance -o yaml`.
// Without this, the failure reason only lives in transient runed logs
// and the record stays at Status=Pending with no detail.
//
// Sets Status=Failed, FailedAt=now, FailureReason, StatusMessage, and
// increments CreateAttempts. Whether the reconciler retries this same
// record in place or tombstones and recreates is decided downstream
// via the ContainerEverCreatedAt gate: nil means create never
// succeeded (precondition failure — operator must fix), non-nil means
// the container existed at some point (transient — recreate).
func (c *instanceController) recordCreateFailure(ctx context.Context, instance *types.Instance, err error, reason string) {
	if instance == nil {
		return
	}
	now := time.Now()
	instance.CreateAttempts++
	instance.StatusMessage = err.Error()
	instance.FailureReason = reason
	instance.UpdatedAt = now

	// Flip to Stalled when retries are exhausted so operators get a
	// clear "stop waiting, take action" signal in `rune get instance`.
	// Mirrors the volume controller's ProvisionRetriesExhausted shape.
	// While Stalled, NextCreateAttemptAt stays nil — the reconciler
	// never auto-retries; only `rune restart instance` or `rune cast`
	// (new generation) re-arms the slot.
	if instance.CreateAttempts >= maxCreateAttempts {
		instance.Status = types.InstanceStatusStalled
		instance.NextCreateAttemptAt = nil
		c.logger.Warn("Instance create retries exhausted; marking Stalled",
			log.Str("instance", instance.ID),
			log.Str("reason", reason),
			log.Int("attempts", instance.CreateAttempts))
	} else {
		instance.Status = types.InstanceStatusFailed
		next := now.Add(createBackoffFor(instance.CreateAttempts))
		instance.NextCreateAttemptAt = &next
	}

	// Deliberately do NOT set FailedAt here. FailedAt marks a *tombstone*
	// — a container that successfully ran and was preserved for
	// postmortem — and is what the retention GC keys off. A
	// stuck-in-create record (ContainerEverCreatedAt == nil) is neither
	// a tombstone nor evictable; the reconciler holds the slot in
	// place via the isInstanceCompatibleWithService gate.
	if updateErr := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); updateErr != nil {
		c.logger.Error("Failed to persist create-failure status on instance",
			log.Str("instance", instance.ID),
			log.Str("reason", reason),
			log.Err(updateErr))
	}
}

// Backoff schedule for CreateInstance retries on a stuck-in-create
// record. Reconciler tick is ~30s (see reconciler.go), so the
// schedule 30s → 1m → 2m → 4m → 5m (cap) gives ~17 min total before
// the 6th attempt flips the record to Stalled. The intent: precondition
// errors that require operator action (StorageClassMissing, missing
// secret, image-pull failures) take minutes to resolve and shouldn't
// hammer the volume controller / image registry every tick.
const (
	maxCreateAttempts = 6
	createBaseBackoff = 30 * time.Second
	createMaxBackoff  = 5 * time.Minute
)

// createBackoffFor returns the delay to wait before the (attempt+1)-th
// retry. Exponential with a cap at createMaxBackoff; attempt is
// 1-based. Mirrors volumeController.backoffFor.
func createBackoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := createBaseBackoff
	for i := 1; i < attempt && d < createMaxBackoff; i++ {
		d *= 2
	}
	if d > createMaxBackoff {
		d = createMaxBackoff
	}
	return d
}

// classifyCreateError maps an error returned during CreateInstance to
// a short, machine-friendly FailureReason slug surfaced on the
// instance record. New reasons can be added without breaking
// consumers — unrecognised errors fall through to "CreateFailed".
func classifyCreateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "StorageClassMissing"):
		return "StorageClassMissing"
	case strings.Contains(msg, "InvalidParameters"):
		return "VolumeInvalidParameters"
	case strings.Contains(msg, "ProvisionRetriesExhausted"):
		return "VolumeProvisionStalled"
	case strings.Contains(msg, "volume ") && strings.Contains(msg, "is not ready"):
		return "VolumeNotReady"
	case strings.Contains(msg, "resolve secret"):
		return "SecretNotFound"
	case strings.Contains(msg, "resolve config"):
		return "ConfigmapNotFound"
	case strings.Contains(msg, "prepare environment variables"):
		return "EnvResolveFailed"
	case strings.Contains(msg, "init steps failed"):
		return "InitStepFailed"
	case strings.Contains(msg, "get runner"):
		return "RunnerUnavailable"
	case strings.Contains(msg, "failed to create instance:"):
		return "RunnerCreateError"
	case strings.Contains(msg, "failed to start instance"):
		return "RunnerStartError"
	}
	return "CreateFailed"
}

// DeleteInstance marks an instance for deletion and cleans up runner resources
func (c *instanceController) DeleteInstance(ctx context.Context, instance *types.Instance) error {
	c.logger.Info("Marking instance for deletion",
		log.Str("instance", instance.ID),
		log.Str("namespace", instance.Namespace),
		log.Str("service", instance.ServiceName))

	// Get the runner for this instance
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return fmt.Errorf("failed to get runner for instance: %w", err)
	}

	// Track failures separately for better error reporting
	failedToStop := false
	failedToRemove := false

	// Try to stop and remove with runner
	if err := runner.Stop(ctx, instance, 10*time.Second); err != nil {
		c.logger.Debug("Failed to stop instance with runner",
			log.Str("instance", instance.ID),
			log.Err(err))
		failedToStop = true
	}

	if err := runner.Remove(ctx, instance, true); err != nil {
		c.logger.Debug("Failed to remove instance with runner",
			log.Str("instance", instance.ID),
			log.Err(err))
		failedToRemove = true
	}

	// Mark the instance as deleted in the store
	originalStatus := instance.Status
	instance.Status = types.InstanceStatusDeleted
	instance.StatusMessage = "Marked for deletion"
	instance.UpdatedAt = time.Now()

	// Store the deletion timestamp for garbage collection
	if instance.Metadata == nil {
		instance.Metadata = &types.InstanceMetadata{}
	}
	deletionTimestamp := time.Now()
	instance.Metadata.DeletionTimestamp = &deletionTimestamp

	if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
		c.logger.Error("Failed to mark instance as deleted",
			log.Str("instance", instance.ID),
			log.Str("from", string(originalStatus)),
			log.Str("to", string(instance.Status)),
			log.Err(err))
	} else {
		c.logger.Info("Instance marked as deleted successfully",
			log.Json("instance", instance.ID))

	}

	// Networking data plane (RUNE-063): drop this instance from the
	// service's published endpoint set and from the local identity
	// table.
	c.republishServiceByInstance(ctx, instance)
	c.republishLocalInstances(ctx)

	// Report any runner errors
	if failedToStop && failedToRemove {
		return fmt.Errorf("failed to both stop and remove instance; instance marked as deleted but resources may remain")
	}

	if failedToStop {
		return fmt.Errorf("failed to stop instance; instance marked as deleted but may still be running")
	}

	if failedToRemove {
		return fmt.Errorf("failed to remove instance; instance marked as deleted but resources may remain")
	}

	return nil
}

// GetInstanceStatus gets the current status of an instance
func (c *instanceController) GetInstanceStatus(ctx context.Context, instance *types.Instance) (*types.InstanceStatusInfo, error) {
	// For now, we'll try to get status from both runners and use the first one that succeeds
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	status, err := runner.Status(ctx, instance)
	if err == nil {
		// Assuming Status returns a string representing the state
		return &types.InstanceStatusInfo{
			Status:     status,
			InstanceID: instance.ID,
			NodeID:     instance.NodeID,
			CreatedAt:  instance.CreatedAt,
		}, nil
	}

	// If runner failed, return the instance status from the store
	return &types.InstanceStatusInfo{
		Status:     instance.Status,
		InstanceID: instance.ID,
		NodeID:     instance.NodeID,
		CreatedAt:  instance.CreatedAt,
	}, nil
}

// GetInstanceLogs gets logs for an instance
func (c *instanceController) GetInstanceLogs(ctx context.Context, instance *types.Instance, opts types.LogOptions) (io.ReadCloser, error) {
	// Try to get logs from both runners and use the first one that succeeds
	_runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	logs, err := _runner.GetLogs(ctx, instance, runner.LogOptions{
		Follow:     opts.Follow,
		Since:      opts.Since,
		Until:      opts.Until,
		Tail:       opts.Tail,
		Timestamps: opts.Timestamps,
	})
	if err == nil {
		return logs, nil
	}

	return nil, fmt.Errorf("failed to get logs for instance %s: %w", instance.ID, err)
}

// Exec executes a command in a running instance
// Dial opens a TCP connection to the given port on the instance's
// running container/process (RUNE-122).
func (c *instanceController) Dial(ctx context.Context, instance *types.Instance, port uint32) (net.Conn, error) {
	c.logger.Debug("Dialing instance",
		log.Str("instance", instance.ID),
		log.Int("port", int(port)))

	if instance.Status != types.InstanceStatusRunning {
		return nil, fmt.Errorf("instance is not running, status: %s", instance.Status)
	}

	_runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	conn, err := _runner.Dial(ctx, instance, port)
	if err != nil {
		return nil, fmt.Errorf("failed to dial instance %s:%d: %w", instance.ID, port, err)
	}
	return conn, nil
}

func (c *instanceController) Exec(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error) {
	c.logger.Debug("Executing command in instance",
		log.Str("instance", instance.ID),
		log.Str("command", strings.Join(options.Command, " ")))

	// Get runner for the instance
	_runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	// Convert orchestrator exec options to runner exec options
	runnerOptions := runner.ExecOptions{
		Command:        options.Command,
		Env:            options.Env,
		WorkingDir:     options.WorkingDir,
		TTY:            options.TTY,
		TerminalWidth:  options.TerminalWidth,
		TerminalHeight: options.TerminalHeight,
	}

	// Create exec session with the runner
	execStream, err := _runner.Exec(ctx, instance, runnerOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to execute command in instance %s: %w", instance.ID, err)
	}

	return execStreamAdapter{execStream}, nil
}

// ExecDebug spawns an ephemeral inspection sidecar for the given (Failed)
// instance and execs options.Command inside it. The sidecar is removed when
// the returned ExecStream is Closed. Used by `rune exec --debug
// <tombstone-id>` to inspect the failed container's image+env+mounts state
// without re-running the failing app.
func (c *instanceController) ExecDebug(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error) {
	c.logger.Info("Spawning debug sidecar",
		log.Str("instance", instance.ID),
		log.Str("command", strings.Join(options.Command, " ")))

	_runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	runnerOptions := runner.ExecOptions{
		Command:        options.Command,
		Env:            options.Env,
		WorkingDir:     options.WorkingDir,
		TTY:            options.TTY,
		TerminalWidth:  options.TerminalWidth,
		TerminalHeight: options.TerminalHeight,
	}

	execStream, err := _runner.RunDebug(ctx, instance, runnerOptions)
	if err != nil {
		return nil, fmt.Errorf("debug exec on instance %s: %w", instance.ID, err)
	}
	return execStreamAdapter{execStream}, nil
}

// RestartInstance restarts an instance with respect to the service's restart policy
func (c *instanceController) RestartInstance(ctx context.Context, instance *types.Instance, reason InstanceRestartReason) error {
	c.logger.Info("Restarting instance",
		log.Str("instance", instance.ID),
		log.Str("reason", string(reason)))

	// First, verify the instance still exists and is in a valid state
	currentInstance, err := c.store.GetInstanceByID(ctx, instance.Namespace, instance.ID)
	if err != nil {
		return fmt.Errorf("instance no longer exists: %w", err)
	}

	// Stuck-in-create record (Failed or Stalled with no container ever
	// created): there's nothing to stop, no tombstone to spawn, and
	// we want to keep the SAME UUID so operators following the slot
	// don't have to chase a moving identifier. Reset the backoff
	// counter so this manual restart gives the record a fresh
	// retry budget, then re-run the create pipeline against the
	// existing record.
	if currentInstance.ContainerEverCreatedAt == nil &&
		(currentInstance.Status == types.InstanceStatusFailed ||
			currentInstance.Status == types.InstanceStatusStalled) {
		var service types.Service
		if err := c.store.Get(ctx, types.ResourceTypeService, currentInstance.Namespace, currentInstance.ServiceName, &service); err != nil {
			return fmt.Errorf("failed to get service for restart: %w", err)
		}
		c.logger.Info("Operator restart on stuck-in-create instance; resetting attempt counter",
			log.Str("instance", currentInstance.ID),
			log.Int("prior_attempts", currentInstance.CreateAttempts))
		currentInstance.CreateAttempts = 0
		return c.RetryCreateInstance(ctx, &service, currentInstance)
	}

	// Check if the instance is in a state that can be restarted
	if currentInstance.Status == types.InstanceStatusDeleted ||
		currentInstance.Status == types.InstanceStatusFailed {
		c.logger.Info("Instance is in terminal state, skipping restart",
			log.Str("instance", instance.ID),
			log.Str("status", string(currentInstance.Status)))
		return nil
	}

	// Get the service to check its restart policy
	var service types.Service
	if err := c.store.Get(ctx, types.ResourceTypeService, instance.Namespace, instance.ServiceName, &service); err != nil {
		return fmt.Errorf("failed to get service for restart policy: %w", err)
	}

	// Manual restarts always override any policy
	if reason == InstanceRestartReasonManual {
		c.logger.Info("Manual restart requested, overriding restart policy",
			log.Str("instance", instance.ID))
	} else {
		// Check restart policy for non-manual restarts
		restartPolicy := types.RestartPolicyAlways // Default to Always
		if service.RestartPolicy != "" {
			restartPolicy = service.RestartPolicy
		}

		// Implement restart policy
		switch restartPolicy {
		case types.RestartPolicyNever:
			// No automatic restarts allowed
			c.logger.Info("Skipping restart due to 'Never' policy",
				log.Str("instance", instance.ID),
				log.Str("reason", string(reason)))
			return nil

		case types.RestartPolicyOnFailure:
			// Only restart if the reason is a failure or health check issue
			isFailureRelated := reason == InstanceRestartReasonFailure || reason == InstanceRestartReasonHealthCheckFailure
			if !isFailureRelated {
				c.logger.Info("Skipping restart due to 'OnFailure' policy with non-failure reason",
					log.Str("instance", instance.ID),
					log.Str("reason", string(reason)))
				return nil
			}
		}
	}

	// Get the appropriate runner
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return fmt.Errorf("failed to get runner for restart: %w", err)
	}

	// Stop the failing container — but preserve it. The new container
	// naming scheme (<namespace>-<service>-<ordinal>-<id_prefix>) means
	// the replacement instance's container gets a fresh ID-suffixed name,
	// so leaving this one stopped-but-present doesn't block anything.
	stopTimeout := 10 * time.Second
	if err := runner.Stop(ctx, instance, stopTimeout); err != nil {
		c.logger.Warn("Failed to stop instance gracefully before tombstone; proceeding anyway",
			log.Str("instance", instance.ID),
			log.Err(err))
	}

	// Mark the failing instance Failed in place. The instance record
	// becomes its own tombstone — same UUID, same Name, container still
	// addressable via instance.ContainerID. Postmortem (rune logs,
	// rune exec --debug) keeps working until the retention GC sweeps it.
	if err := c.markInstanceFailedInPlace(ctx, instance, reason); err != nil {
		c.logger.Error("Failed to mark failing instance as Failed",
			log.Str("instance", instance.ID),
			log.Err(err))
		// Keep going — the replacement still needs to spawn.
	}

	// Spawn a fresh replacement instance with the same logical Name
	// (e.g. "landing-0") but a brand-new UUID. Reconciler's slot lookup
	// filters Failed records, so it sees the slot as unfilled and our
	// new record claims it. Same Name across the tombstone + the live
	// replacement is fine — they're disambiguated by Status and ID.
	// `service` was already loaded above for the restart-policy check.
	replacement, err := c.CreateInstance(ctx, &service, instance.Name)
	if err != nil {
		return fmt.Errorf("failed to spawn replacement for %s: %w", instance.ID, err)
	}

	// Carry restart counters forward from the tombstone so operators
	// can still see "this slot has restarted N times" — RestartCount
	// lives on the live instance's metadata, not on the tombstone's.
	if replacement.Metadata == nil {
		replacement.Metadata = &types.InstanceMetadata{}
	}
	priorRestarts := 0
	if instance.Metadata != nil {
		priorRestarts = instance.Metadata.RestartCount
	}
	replacement.Metadata.RestartCount = priorRestarts + 1
	if err := c.store.Update(ctx, types.ResourceTypeInstance, replacement.Namespace, replacement.ID, replacement); err != nil {
		c.logger.Warn("Failed to carry restart counter to replacement",
			log.Str("replacement", replacement.ID),
			log.Err(err))
	}

	c.logger.Info("Restart complete: tombstoned + replaced",
		log.Str("tombstone", instance.ID),
		log.Str("replacement", replacement.ID),
		log.Str("reason", string(reason)),
		log.Int("restart_count", replacement.Metadata.RestartCount))

	return nil
}

// collectRunningInstances gathers all running instances from all runners
func (c *instanceController) collectRunningInstances(ctx context.Context) (map[string]*RunningInstance, error) {
	instances := make(map[string]*RunningInstance)

	// Collect instances from docker runner
	dockerRunner, err := c.runnerManager.GetDockerRunner()
	if err != nil {
		return nil, fmt.Errorf("failed to get docker runner: %w", err)
	}
	dockerInstances, err := dockerRunner.List(ctx, "")
	if err != nil {
		c.logger.Error("Failed to list docker instances", log.Err(err))
		// Continue with other runners even if one fails
	} else {
		for _, instance := range dockerInstances {
			instances[instance.ID] = &RunningInstance{
				Instance:   instance,
				IsOrphaned: true, // Mark as orphaned initially, will be updated during reconciliation
				Runner:     dockerRunner.Type(),
			}
		}
	}

	// Collect instances from process runner
	processRunner, err := c.runnerManager.GetProcessRunner()
	if err != nil {
		return nil, fmt.Errorf("failed to get process runner: %w", err)
	}
	processInstances, err := processRunner.List(ctx, "")
	if err != nil {
		c.logger.Error("Failed to list process instances", log.Err(err))
		// Continue with other runners even if one fails
	} else {
		for _, instance := range processInstances {
			instances[instance.ID] = &RunningInstance{
				Instance:   instance,
				IsOrphaned: true, // Mark as orphaned initially, will be updated during reconciliation
				Runner:     processRunner.Type(),
			}
		}
	}

	return instances, nil
}

// isInstanceCompatibleWithService checks if an instance is compatible with a service
func (c *instanceController) isInstanceCompatibleWithService(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string) {
	// Check if the instance belongs to the correct service
	if instance.ServiceID != service.ID {
		return false, "instance belongs to different service"
	}

	// Stuck-in-create record: Status=Failed (or Stalled) but a
	// container was never successfully created for this UUID
	// (precondition failure such as StorageClassMissing, missing
	// secret, image-pull error). The slot is legitimately held by
	// this record — returning false would trigger
	// tombstone+recreate-with-new-UUID every reconcile tick, the
	// exact churn that
	// _docs/bugs-reporting/RUNE-BUG-RECONCILER-CHURN-ON-STABLE-PRECONDITION-FAILURE.md
	// describes. Return true so the reconciler leaves the record in
	// place; the reconciler's retry-in-place branch handles backoff
	// (Failed) or holds-without-retry (Stalled — operator must run
	// `rune restart instance` or `rune cast` to re-arm).
	if instance.ContainerEverCreatedAt == nil &&
		(instance.Status == types.InstanceStatusFailed || instance.Status == types.InstanceStatusStalled) {
		return true, ""
	}

	// Check if the instance is in a failed state
	if instance.Status == types.InstanceStatusFailed ||
		instance.Status == types.InstanceStatusExited ||
		instance.Status == types.InstanceStatusUnknown {
		return false, fmt.Sprintf("instance is in failed state: %s", string(instance.Status))
	}

	// Check the service generation
	if instance.Metadata != nil {
		if instanceGen := instance.Metadata.ServiceGeneration; instanceGen != 0 {
			// Convert instance's stored generation to int64
			if instanceGen < service.Metadata.Generation {
				c.logger.Debug("Service generation changed, instance needs recreation",
					log.Str("instance", instance.ID),
					log.Int64("instance_generation", instanceGen),
					log.Int64("service_generation", service.Metadata.Generation))
				return false, fmt.Sprintf("service generation changed: %d -> %d", instanceGen, service.Metadata.Generation)
			}
		} else {
			// If instance doesn't have a stored generation but service has one, recreate
			if service.Metadata.Generation > 0 {
				c.logger.Debug("Instance missing service generation, needs recreation",
					log.Str("instance", instance.ID),
					log.Int64("service_generation", service.Metadata.Generation))
				return false, "instance missing service generation"
			}
		}
	}

	// Get the current runner for the instance
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return false, fmt.Sprintf("failed to get runner: %v", err)
	}

	// Check if the instance still exists in the runner
	status, err := runner.Status(ctx, instance)
	if err != nil {
		return false, fmt.Sprintf("instance not found in runner: %v", err)
	}

	// If instance exists but is in a terminal state, it's incompatible
	if status == types.InstanceStatusExited || status == types.InstanceStatusFailed {
		return false, "instance is in terminal state in the runner"
	}

	// For Docker-based instances, perform additional checks
	if instance.ContainerID != "" && service.Runtime == "docker" {
		// Check if image has changed
		// This would require the Instance to store the image it was created with
		if instance.Metadata != nil {
			// Look for stored image information in the metadata
			if instance.Metadata.Image != "" {
				if instance.Metadata.Image != service.Image {
					return false, fmt.Sprintf("image changed: %s -> %s", instance.Metadata.Image, service.Image)
				}
			} else {
				// If we can't determine the original image, be cautious and recreate
				c.logger.Debug("Cannot determine original image for instance, assuming incompatible")
				return false, "cannot determine original image"
			}
		}

		// Check for significant resource changes
		if service.Resources.CPU.Limit != "" || service.Resources.Memory.Limit != "" {
			// If instance doesn't have resources configured but service does
			if instance.Resources == nil ||
				(instance.Resources.CPU.Limit != service.Resources.CPU.Limit) ||
				(instance.Resources.Memory.Limit != service.Resources.Memory.Limit) {
				return false, "resource requirements changed"
			}
		}

		// Check for port mapping changes
		// This is more complex and would need to compare port configurations

		// Check for significant environment changes
		// Partial environment changes might be fine, but essential vars should match
		if len(service.Env) > 0 {
			for key, value := range service.Env {
				// Skip RUNE internal environment variables
				if len(key) > 5 && key[:5] == "RUNE_" {
					continue
				}

				instanceValue, exists := instance.Environment[key]
				if !exists || instanceValue != value {
					return false, fmt.Sprintf("environment variable %s changed or missing", key)
				}
			}
		}
	}

	// For process-based instances, perform process-specific checks
	if instance.PID != 0 && service.Runtime == "process" {
		// Check command consistency
		if instance.Process != nil && service.Process != nil {
			if instance.Process.Command != service.Process.Command ||
				!areStringSlicesEqual(instance.Process.Args, service.Process.Args) {
				return false, "process command or arguments changed"
			}

			// Check for working directory changes
			if instance.Process.WorkingDir != service.Process.WorkingDir {
				return false, "process working directory changed"
			}
		}
	}

	// If we get here, the instance is compatible
	return true, ""
}

// prepareEnvVars prepares environment variables for an instance
func (c *instanceController) prepareEnvVars(ctx context.Context, service *types.Service, instance *types.Instance) (map[string]string, error) {
	envVars := make(map[string]string)

	// 1) Import from envFrom sources in order
	for _, src := range service.EnvFrom {
		var data map[string]string
		if src.SecretName != "" {
			sec, err := c.secretRepo.Get(ctx, src.Namespace, src.SecretName)
			if err != nil {
				return nil, fmt.Errorf("envFrom secret %s.%s: %w", src.Namespace, src.SecretName, err)
			}
			data = sec.Data
		} else if src.ConfigmapName != "" {
			cfg, err := c.configRepo.Get(ctx, src.Namespace, src.ConfigmapName)
			if err != nil {
				return nil, fmt.Errorf("envFrom configmap %s.%s: %w", src.Namespace, src.ConfigmapName, err)
			}
			data = cfg.Data
		}
		if data == nil {
			continue
		}
		for k, v := range data {
			key := k
			if src.Prefix != "" {
				key = src.Prefix + key
			}
			if !isValidEnvKey(key) {
				return nil, fmt.Errorf("invalid environment variable name from envFrom: %s", key)
			}
			envVars[key] = v
		}
	}

	// 2) Add service-defined environment variables with interpolation (override imported)
	for key, value := range service.Env {
		resolved, err := c.interpolateEnv(ctx, value, service.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to interpolate env %s: %w", key, err)
		}
		envVars[key] = resolved
	}

	// Add built-in environment variables
	envVars["RUNE_SERVICE_NAME"] = service.Name
	envVars["RUNE_SERVICE_NAMESPACE"] = service.Namespace
	envVars["RUNE_INSTANCE_ID"] = instance.ID

	// Add normalized environment variables (for compatibility)
	serviceName := strings.ToUpper(service.Name)
	serviceName = strings.ReplaceAll(serviceName, "-", "_")

	envVars[fmt.Sprintf("%s_SERVICE_HOST", serviceName)] = fmt.Sprintf("%s.%s.rune", service.Name, service.Namespace)

	// Add port-related environment variables
	for _, port := range service.Ports {
		portName := strings.ToUpper(port.Name)
		portName = strings.ReplaceAll(portName, "-", "_")

		envVars[fmt.Sprintf("%s_SERVICE_PORT_%s", serviceName, portName)] = fmt.Sprintf("%d", port.Port)

		// If this is the first port, also set the default port
		if len(envVars[fmt.Sprintf("%s_SERVICE_PORT", serviceName)]) == 0 {
			envVars[fmt.Sprintf("%s_SERVICE_PORT", serviceName)] = fmt.Sprintf("%d", port.Port)
		}
	}

	return envVars, nil
}

// isValidEnvKey checks if key matches ^[A-Z_][A-Z0-9_]*$
func isValidEnvKey(key string) bool {
	if len(key) == 0 {
		return false
	}
	// First char: A-Z or _
	c0 := key[0]
	if !((c0 >= 'A' && c0 <= 'Z') || c0 == '_') {
		return false
	}
	for i := 1; i < len(key); i++ {
		c := key[i]
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// interpolateEnv resolves template variables in the format {{type:reference}} using the controller's repos
func (c *instanceController) interpolateEnv(ctx context.Context, value, defaultNamespace string) (string, error) {
	// Check if the value contains template syntax
	if !strings.Contains(value, "{{") || !strings.Contains(value, "}}") {
		// No template syntax, return as-is
		return value, nil
	}

	// Find all template variables and replace them
	result := value
	start := 0
	for {
		openIdx := strings.Index(result[start:], "{{")
		if openIdx == -1 {
			break
		}
		openIdx += start

		closeIdx := strings.Index(result[openIdx:], "}}")
		if closeIdx == -1 {
			break
		}
		closeIdx += openIdx

		// Extract the template variable content and trim whitespace inside the braces
		templateVar := trimWhitespaces(result[openIdx+2 : closeIdx])

		// Resolve the template variable
		resolvedValue, err := c.resolveTemplateVariable(ctx, templateVar, defaultNamespace)
		if err != nil {
			return "", fmt.Errorf("failed to resolve template variable %s: %w", templateVar, err)
		}

		// Replace the template variable with the resolved value
		result = result[:openIdx] + resolvedValue + result[closeIdx+2:]

		// Update start position for next iteration
		start = openIdx + len(resolvedValue)
	}

	return result, nil
}

// resolveTemplateVariable parses and resolves a single template variable
func (c *instanceController) resolveTemplateVariable(ctx context.Context, templateVar, defaultNamespace string) (string, error) {
	// Parse the template variable as a resource reference
	resourceRef, err := types.ParseResourceRefWithDefaultNamespace(templateVar, defaultNamespace)
	if err != nil {
		return "", fmt.Errorf("failed to parse template variable %s: %w", templateVar, err)
	}

	// Fail fast if no key is specified - we need a key to extract a value
	if !resourceRef.HasKey() {
		return "", fmt.Errorf("template variable must include a key for interpolation: %s", templateVar)
	}

	// Resolve the resource reference
	switch resourceRef.Type {
	case types.ResourceTypeSecret:
		return c.resolveSecretValue(ctx, resourceRef)
	case types.ResourceTypeConfigmap:
		return c.resolveConfigmapValue(ctx, resourceRef)
	default:
		return "", fmt.Errorf("unsupported resource type %s in template variable: %s", resourceRef.Type, templateVar)
	}
}

// resolveSecretValue fetches and extracts a value from a secret
func (c *instanceController) resolveSecretValue(ctx context.Context, resourceRef types.ResourceRef) (string, error) {
	sec, err := c.secretRepo.Get(ctx, resourceRef.Namespace, resourceRef.Name)
	if err != nil {
		return "", fmt.Errorf("get secret %s.%s: %w", resourceRef.Namespace, resourceRef.Name, err)
	}
	if sec.Data == nil {
		return "", fmt.Errorf("secret %s.%s has no data", resourceRef.Namespace, resourceRef.Name)
	}
	v, ok := sec.Data[resourceRef.Key]
	if !ok {
		return "", fmt.Errorf("key %s not found in secret %s.%s", resourceRef.Key, resourceRef.Namespace, resourceRef.Name)
	}
	return v, nil
}

// resolveConfigmapValue fetches and extracts a value from a configmap
func (c *instanceController) resolveConfigmapValue(ctx context.Context, resourceRef types.ResourceRef) (string, error) {
	cfg, err := c.configRepo.Get(ctx, resourceRef.Namespace, resourceRef.Name)
	if err != nil {
		return "", fmt.Errorf("get configmap %s.%s: %w", resourceRef.Namespace, resourceRef.Name, err)
	}
	if cfg.Data == nil {
		return "", fmt.Errorf("configmap %s.%s has no data", resourceRef.Namespace, resourceRef.Name)
	}
	v, ok := cfg.Data[resourceRef.Key]
	if !ok {
		return "", fmt.Errorf("key %s not found in configmap %s.%s", resourceRef.Key, resourceRef.Namespace, resourceRef.Name)
	}
	return v, nil
}

// resolveMounts resolves secret and config mounts for an instance by fetching the actual data
func (c *instanceController) resolveMounts(ctx context.Context, service *types.Service, instance *types.Instance) error {
	// Initialize metadata if not present
	if instance.Metadata == nil {
		instance.Metadata = &types.InstanceMetadata{}
	}

	// Resolve secret mounts
	if len(service.SecretMounts) > 0 {
		instance.Metadata.SecretMounts = make([]types.ResolvedSecretMount, 0, len(service.SecretMounts))

		for _, mount := range service.SecretMounts {
			// Determine secret name; default to mount.Name if SecretName is omitted
			secretName := mount.SecretName
			if secretName == "" {
				secretName = mount.Name
			}
			// Get the secret from the store
			secret, err := c.secretRepo.Get(ctx, service.Namespace, secretName)
			if err != nil {
				return fmt.Errorf("failed to get secret %s for mount %s: %w", secretName, mount.Name, err)
			}

			// Create resolved mount
			resolvedMount := types.ResolvedSecretMount{
				Name:      mount.Name,
				MountPath: mount.MountPath,
				Data:      secret.Data,
				Items:     mount.Items,
			}

			instance.Metadata.SecretMounts = append(instance.Metadata.SecretMounts, resolvedMount)
		}
	}

	// Resolve config mounts
	if len(service.ConfigmapMounts) > 0 {
		instance.Metadata.ConfigmapMounts = make([]types.ResolvedConfigmapMount, 0, len(service.ConfigmapMounts))

		for _, mount := range service.ConfigmapMounts {
			// Determine config name; default to mount.Name if ConfigName is omitted
			configName := mount.ConfigmapName
			if configName == "" {
				configName = mount.Name
			}
			// Get the config from the store
			config, err := c.configRepo.Get(ctx, service.Namespace, configName)
			if err != nil {
				return fmt.Errorf("failed to get config %s for mount %s: %w", configName, mount.Name, err)
			}

			// Create resolved mount
			resolvedMount := types.ResolvedConfigmapMount{
				Name:      mount.Name,
				MountPath: mount.MountPath,
				Data:      config.Data,
				Items:     mount.Items,
			}

			instance.Metadata.ConfigmapMounts = append(instance.Metadata.ConfigmapMounts, resolvedMount)
		}
	}

	// Resolve volume mounts.
	//
	// - Claim: looks up an existing Volume by name (cross-namespace via
	//   "ns/name"); fails if not yet Available.
	// - ClaimTemplate: idempotently creates a per-instance Volume named
	//   "<mount.Name>-<service.Name>-<ordinal>" with OwnerService set,
	//   then waits for the VolumeController to provision it.
	//
	// Mounts that resolve to a not-yet-Available volume surface as a
	// reconcile error so the instance status reports the cause; the
	// service reconciler retries on the next tick.
	if len(service.Volumes) > 0 {
		instance.Metadata.VolumeMounts = make([]types.ResolvedVolumeMount, 0, len(service.Volumes))
		for _, m := range service.Volumes {
			resolved, err := c.resolveVolumeMount(ctx, service, instance, m)
			if err != nil {
				return fmt.Errorf("failed to resolve volume mount %q: %w", m.Name, err)
			}
			instance.Metadata.VolumeMounts = append(instance.Metadata.VolumeMounts, resolved)
		}
	}

	return nil
}

// resolveVolumeMount converts a Service.VolumeMount into the runner-facing
// ResolvedVolumeMount by looking up (or auto-provisioning, for the
// ClaimTemplate form) the bound Volume and using its Handle as the
// host-side bind source.
func (c *instanceController) resolveVolumeMount(ctx context.Context, service *types.Service, instance *types.Instance, m types.VolumeMount) (types.ResolvedVolumeMount, error) {
	switch {
	case m.Claim != nil && m.ClaimTemplate != nil:
		return types.ResolvedVolumeMount{}, fmt.Errorf("volume mount %q sets both claim and claimTemplate; pick one", m.Name)
	case m.Claim == nil && m.ClaimTemplate == nil:
		return types.ResolvedVolumeMount{}, fmt.Errorf("volume mount %q sets neither claim nor claimTemplate", m.Name)
	}

	// Resolve the Volume reference: ClaimTemplate drives idempotent
	// auto-creation, Claim is a straight lookup.
	var ns, name string
	if m.ClaimTemplate != nil {
		ns = service.Namespace
		ordinal, ok := instanceOrdinal(service.Name, instance.Name)
		if !ok {
			return types.ResolvedVolumeMount{}, fmt.Errorf("cannot derive ordinal from instance name %q for service %q", instance.Name, service.Name)
		}
		name = fmt.Sprintf("%s-%s-%d", m.Name, service.Name, ordinal)
		if err := c.ensureClaimTemplateVolume(ctx, service, m, name, ordinal); err != nil {
			return types.ResolvedVolumeMount{}, fmt.Errorf("ensure claim-template volume %s/%s: %w", ns, name, err)
		}
	} else {
		ns = service.Namespace
		name = m.Claim.Name
		if idx := strings.Index(name, "/"); idx > 0 {
			ns, name = name[:idx], name[idx+1:]
		}
	}

	var vol types.Volume
	if err := c.store.Get(ctx, types.ResourceTypeVolume, ns, name, &vol); err != nil {
		return types.ResolvedVolumeMount{}, fmt.Errorf("get volume %s/%s: %w", ns, name, err)
	}

	if vol.Status != types.VolumeStatusAvailable && vol.Status != types.VolumeStatusBound {
		return types.ResolvedVolumeMount{}, fmt.Errorf("volume %s/%s is not ready (status=%s, reason=%q)", ns, name, vol.Status, vol.Reason)
	}

	// Bind the volume to this node + consuming instance so the agent-
	// side volumes Subsystem will Attach + Mount it. The Subsystem
	// gates on BoundNode == nodeID (see internal/agent/volumes/
	// subsystem.go shouldMount). BoundClaim records which instance
	// currently owns the binding — refreshed on every instance change
	// (e.g. `rune restart` cycling 1→0→1) so the row doesn't keep
	// pointing at a Deleted instance after a restart.
	if c.nodeID != "" {
		newClaim := service.Namespace + "/" + instance.Name
		if vol.BoundNode != c.nodeID || vol.BoundClaim != newClaim {
			vol.BoundNode = c.nodeID
			vol.BoundClaim = newClaim
			vol.UpdatedAt = time.Now().UTC()
			if err := c.store.Update(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, &vol); err != nil {
				return types.ResolvedVolumeMount{}, fmt.Errorf("bind volume %s/%s to node %s: %w", ns, name, c.nodeID, err)
			}
		}
	}

	// Resolve the bind source. When a MountResolver is wired (production
	// runed), the agent-side Subsystem owns the host-path mapping; we
	// require it to have reported a target before launching, because for
	// any driver where Handle is not a host path (do-volume, future cloud
	// drivers) a bare Handle is not a valid Docker bind source. The error
	// is transient — the service reconciler retries until the Subsystem
	// has finished Attach + Mount.
	//
	// When no MountResolver is wired (dev/standalone, tests), fall back
	// to Volume.Handle. That's the historical behaviour and is correct
	// for the in-tree local / local-host drivers where Handle == host
	// path.
	var source string
	if c.mountResolver != nil {
		target, ok := c.mountResolver.MountTargetFor(vol.ID)
		if !ok {
			return types.ResolvedVolumeMount{}, fmt.Errorf("volume %s/%s not yet mounted on node %s (will retry)", ns, name, c.nodeID)
		}
		source = target
	} else {
		source = vol.Handle
	}
	if source == "" {
		return types.ResolvedVolumeMount{}, fmt.Errorf("volume %s/%s has no mount source", ns, name)
	}

	// Apply fsUser / fsGroup / fsMode to the mount root, idempotently.
	// Solves the "fresh ext4 owned by root, container runs as uid N,
	// EACCES on first write" pattern without an init-step chown. Only
	// runs when the operator opted in — absent fields are a no-op so
	// local-host mounts (operator-managed paths) aren't stomped on.
	if err := applyFSOwnership(source, m.FSUser, m.FSGroup, m.FSMode); err != nil {
		return types.ResolvedVolumeMount{}, fmt.Errorf("apply fs ownership on %s (volume %s/%s): %w", source, ns, name, err)
	}

	return types.ResolvedVolumeMount{
		Name:            m.Name,
		MountPath:       m.MountPath,
		Source:          source,
		VolumeName:      name,
		VolumeNamespace: ns,
		ReadOnly:        m.ReadOnly,
		SubPath:         m.SubPath,
	}, nil
}

// instanceOrdinal extracts the trailing integer ordinal from an
// instance name produced by generateInstanceName ("<service>-<n>").
// Returns false when the instance name does not match the expected
// shape (e.g. ad-hoc names from tests or RecreateInstance with a
// reused name).
func instanceOrdinal(serviceName, instanceName string) (int, bool) {
	prefix := serviceName + "-"
	if !strings.HasPrefix(instanceName, prefix) {
		return 0, false
	}
	suffix := instanceName[len(prefix):]
	if suffix == "" {
		return 0, false
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ensureClaimTemplateVolume creates the per-replica Volume row from a
// ClaimTemplate the first time it is observed. It is idempotent: a
// pre-existing volume with the same namespace+name is left alone (the
// VolumeController owns subsequent mutations).
func (c *instanceController) ensureClaimTemplateVolume(ctx context.Context, service *types.Service, m types.VolumeMount, name string, ordinal int) error {
	ns := service.Namespace
	var existing types.Volume
	if err := c.store.Get(ctx, types.ResourceTypeVolume, ns, name, &existing); err == nil {
		return nil
	}

	tpl := m.ClaimTemplate
	now := time.Now().UTC()
	vol := &types.Volume{
		ID:               name,
		Name:             name,
		Namespace:        ns,
		StorageClassName: tpl.StorageClassName,
		Size:             tpl.Size,
		AccessMode:       tpl.AccessMode,
		Parameters:       tpl.Parameters,
		ReclaimPolicy:    tpl.ReclaimPolicy,
		OwnerService:     fmt.Sprintf("%s/%s", ns, service.Name),
		BoundClaim:       fmt.Sprintf("%s/%s/%d", service.Name, m.Name, ordinal),
		Status:           types.VolumeStatusPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := c.store.Create(ctx, types.ResourceTypeVolume, ns, name, vol); err != nil {
		// A racing reconcile may have created it; tolerate that.
		var exists types.Volume
		if getErr := c.store.Get(ctx, types.ResourceTypeVolume, ns, name, &exists); getErr == nil {
			return nil
		}
		return err
	}
	return nil
}
