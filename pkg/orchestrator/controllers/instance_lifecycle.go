// Package controllers — Instance lifecycle: create/retry/recreate/update/stop/delete/restart
// and runner-state collection. Split from instance_controller.go (RUNE-311).
package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

func (c *InstanceController) GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	return c.store.GetInstanceByID(ctx, namespace, instanceID)
}

func (c *InstanceController) ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	var instances []*types.Instance
	err := c.store.List(ctx, types.ResourceTypeInstance, namespace, &instances)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	return instances, nil
}

func (c *InstanceController) ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
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

	// filter instances by running instances. An empty namespace means "all
	// namespaces" (matching the store.List semantics above) — the agent log
	// forwarder relies on this to tap running instances across every namespace.
	runningInstancesPointers := make([]*types.Instance, 0, len(runningInstances))
	for _, instance := range runningInstances {
		for i := range storeInstances {
			storeInstance := storeInstances[i]
			if instance.Instance.ID == storeInstance.ID && (namespace == "" || storeInstance.Namespace == namespace) {
				runningInstancesPointers = append(runningInstancesPointers, &storeInstance)
			}
		}
	}

	return runningInstancesPointers, nil
}

// CreateInstance creates a new instance for a service
// This would be simplified to only handle the pure creation case
func (c *InstanceController) CreateInstance(ctx context.Context, service *types.Service, instanceName string, ordinal int) (*types.Instance, error) {
	c.logger.Info("Creating new instance",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Str("instance", instanceName))

	// Create instance object
	instance := &types.Instance{
		ID:          uuid.New().String(),
		Name:        instanceName,
		Ordinal:     ordinal,
		Namespace:   service.Namespace,
		ServiceName: service.Name,
		ServiceID:   service.ID,
		NodeID:      "local",
		Status:      types.InstanceStatusPending,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Metadata:    &types.InstanceMetadata{},
	}

	// Denormalize the parent service's user labels onto the instance so they
	// become queryable LogQL stream dimensions (e.g. {app="web"}) and the
	// substrate for future affinity/topology-spread scheduling. A copy keeps the
	// instance independent of later mutations to the service spec.
	//
	// TODO(scheduler): once a placement scheduler assigns NodeID (instead of the
	// hardcoded "local"), merge the chosen Node's topology labels here too, so
	// instances carry region/zone for region-aware log queries and spread.
	if len(service.Labels) > 0 {
		instance.Labels = make(map[string]string, len(service.Labels))
		for k, v := range service.Labels {
			instance.Labels[k] = v
		}
	}

	// Propagate resolved resource constraints from service to instance
	// Use a pointer so runners can access limits/requests directly
	instance.Resources = &service.Resources

	// Propagate SecurityContext from service to instance so the runner
	// can apply seccomp / capabilities / privileged to the main
	// container. Init steps carry their own SecurityContext.
	instance.SecurityContext = service.SecurityContext

	// Store the service TEMPLATE generation in instance metadata — the
	// counter that only advances on spec/template changes (cast), never on
	// scale (issue #142). The compatibility check compares against it, so a
	// later scale-up doesn't make this instance look stale. ServiceMetadata
	// may be nil for hand-built services in tests; treat as generation 0.
	if service.Metadata != nil {
		instance.Metadata.ServiceGeneration = service.Metadata.TemplateGeneration
	}

	// Propagate ports and expose spec for runner use and later status
	if len(service.Ports) > 0 {
		instance.Metadata.Ports = append(instance.Metadata.Ports, service.Ports...)
	}
	if service.Expose != nil {
		instance.Metadata.Expose = service.Expose
	}
	if service.Metadata != nil {
		c.logger.Debug("Storing service template generation in instance",
			log.Str("instance", instanceName),
			log.Int64("template_generation", service.Metadata.TemplateGeneration))
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
func (c *InstanceController) RetryCreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	if instance == nil {
		return fmt.Errorf("retry: nil instance")
	}
	c.logger.Info("Retrying create on stuck-in-create instance",
		log.Str("service", service.Name),
		log.Str("instance", instance.Name),
		log.Str("id", instance.ID),
		log.Int("attempt", instance.CreateAttempts+1))

	applyInstanceStatus(instance, types.InstanceStatusPending, "", "")
	instance.NextCreateAttemptAt = nil
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
func (c *InstanceController) runCreateAttempt(ctx context.Context, service *types.Service, instance *types.Instance) error {
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
		instance.Metadata.ImagePullAnonymous = service.ImagePullAnonymous
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

	// Status transition after a successful container start.
	//
	// Without a readiness probe we promote straight to Running — that's
	// what "the runtime accepted the container" means in the absence of
	// any other signal.
	//
	// With a readiness probe we hold at Starting until the health
	// controller observes the first readiness pass and promotes us. The
	// previous behaviour (always flip to Running on runner.Start
	// success) was operator-confusing on services like prod/gateway
	// that show `Running` for ~30s while the app boots, then get
	// SIGKILL'd by the liveness probe — the "Running" status was real
	// but didn't mean "ready to serve traffic." Matches K8s semantics
	// where Pod.Phase=Running ≠ Ready.
	if service.Health != nil && service.Health.Readiness != nil {
		applyInstanceStatus(instance, types.InstanceStatusStarting, "", "Waiting for readiness probe")
		c.emit(types.EventLevelInfo, instance, "", "Container started; waiting for readiness probe")
	} else {
		applyInstanceStatus(instance, types.InstanceStatusRunning, "", "Created successfully")
		c.emit(types.EventLevelInfo, instance, "", "Instance running")
	}
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
func (c *InstanceController) RecreateInstance(ctx context.Context, service *types.Service, existingInstance *types.Instance) (*types.Instance, error) {
	instanceName := existingInstance.Name
	c.logger.Info("Recreating instance",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Str("instance", instanceName))

	// Delete the existing instance
	if err := c.DeleteInstance(ctx, existingInstance); err != nil {
		return nil, fmt.Errorf("failed to delete instance for recreation: %w", err)
	}

	// Create a new instance — same logical slot, so carry the ordinal forward.
	return c.CreateInstance(ctx, service, instanceName, existingInstance.Ordinal)
}

// UpdateInstance updates an existing instance
func (c *InstanceController) UpdateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	c.logger.Debug("Checking instance for updates",
		log.Str("instance", instance.ID),
		log.Str("service", service.Name))

	// Get current runner for this instance
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return fmt.Errorf("failed to get runner for instance: %w", err)
	}

	// Classify the instance against the current service definition.
	//
	// Only BROKEN instances get the recreation error here. An OUTDATED
	// instance is serving fine and its replacement is the update budget's
	// decision, not this function's (RUNE-042 §6.3): UpdateInstance is
	// called on every reconcile for every surviving instance, so returning
	// the recreation error for outdated ones would destroy them through this
	// path regardless of any budget — the budget would govern nothing.
	//
	// This is behaviour-preserving today: the reconciler's ensure* loops
	// still delete outdated instances themselves (they act on the boolean
	// view before ever calling here), so the leave-alone branch only absorbs
	// the mid-reconcile race this used to recreate on. Phase 4 moves that
	// decision into the planner and this branch becomes load-bearing.
	verdict := c.classifyInstance(ctx, instance, service)
	switch verdict.Class {
	case CompatBroken:
		c.logger.Info("Instance is broken, recreation required",
			log.Str("instance", instance.ID),
			log.Str("service", service.Name),
			log.Str("reason", verdict.Reason))
		// Stop and return an error indicating that recreation is needed;
		// the caller handles the recreation.
		return fmt.Errorf("instance %s requires recreation to update: %s", instance.ID, verdict.Reason)
	case CompatOutdated:
		c.logger.Debug("Instance is outdated; leaving replacement to the update planner",
			log.Str("instance", instance.ID),
			log.Str("service", service.Name),
			log.Str("reason", verdict.Reason))
		return nil
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

	// Check if the service TEMPLATE generation has changed. Instances key on
	// TemplateGeneration (not Generation, which also bumps on scale) so this
	// in-place sync only fires on genuine template drift (issue #142).
	generationUpdated := instance.Metadata.ServiceGeneration != service.Metadata.TemplateGeneration
	if generationUpdated {
		instance.Metadata.ServiceGeneration = service.Metadata.TemplateGeneration
		instanceUpdated = true
		c.logger.Debug("Updating service template generation in instance",
			log.Str("instance", instance.ID),
			log.Int64("template_generation", service.Metadata.TemplateGeneration))
	}

	// Update timestamp only if we made meaningful changes
	if instanceUpdated {
		instance.UpdatedAt = time.Now()
	}

	// If we've made any updates to the instance object, save it back to the
	// store. Apply ONLY the spec-sync fields, atomically, on the fresh record —
	// and skip entirely if the instance is no longer Running (e.g. the health
	// controller just marked it Failed). A full-object write here would revert
	// that concurrent status transition, resurrecting a Failed instance
	// (RFC #129 Phase 1c). This in-place update is defined only for Running
	// instances anyway (checked against the runner above).
	if instanceUpdated {
		var fresh types.Instance
		if err := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, &fresh, func() error {
			if fresh.Status != types.InstanceStatusRunning {
				return store.ErrSkipUpdate
			}
			fresh.Environment = instance.Environment
			if fresh.Metadata == nil {
				fresh.Metadata = &types.InstanceMetadata{}
			}
			fresh.Metadata.ServiceGeneration = instance.Metadata.ServiceGeneration
			fresh.StatusMessage = instance.StatusMessage
			fresh.UpdatedAt = instance.UpdatedAt
			return nil
		}, store.WithReconciler()); err != nil {
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
func (c *InstanceController) StopInstance(ctx context.Context, instance *types.Instance) error {
	c.logger.Info("Stopping instance",
		log.Str("instance", instance.ID))

	// Get the runner for this instance
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return fmt.Errorf("failed to get runner for instance: %w", err)
	}

	// Withdraw from the dataplane BEFORE stopping (RUNE-042 §4, Phase 0):
	// flip to Terminating so republishService excludes this instance,
	// publish the shrunken endpoint set, drain, and only then SIGTERM. On
	// runner failure the flip is reverted below so the record keeps telling
	// the truth (the container is still running). Batch teardowns
	// (WithdrawServiceInstances) pre-flip and take one shared drain, which
	// makes wasServing false here and skips the per-instance wait.
	wasServing := instance.Status == types.InstanceStatusRunning
	var owningService *types.Service
	if wasServing {
		var fresh types.Instance
		if err := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, &fresh, func() error {
			if fresh.Status != types.InstanceStatusRunning {
				return store.ErrSkipUpdate
			}
			fresh.Status = types.InstanceStatusTerminating
			fresh.StatusMessage = "Stopping"
			fresh.UpdatedAt = time.Now()
			return nil
		}); err != nil {
			c.logger.Warn("Failed to mark instance Terminating before stop",
				log.Str("instance", instance.ID),
				log.Err(err))
		} else {
			instance.Status = types.InstanceStatusTerminating
		}
		if instance.ServiceName != "" {
			var svc types.Service
			if err := c.store.Get(ctx, types.ResourceTypeService, instance.Namespace, instance.ServiceName, &svc); err == nil {
				owningService = &svc
				c.republishService(ctx, owningService)
				c.republishLocalInstances(ctx)
			}
		}
	}
	c.drainAfterWithdraw(ctx, owningService, wasServing, instance.ID)

	// Stop the instance with the runner
	if err := runner.Stop(ctx, instance, 10*time.Second); err != nil {
		c.logger.Error("Failed to stop instance with runner",
			log.Str("instance", instance.ID),
			log.Err(err))
		// Revert the withdrawal flip: the container is still running and
		// the record must say so. Best-effort — the next reconcile
		// converges either way — and the republish restores the endpoint.
		if wasServing {
			var fresh types.Instance
			if rerr := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, &fresh, func() error {
				if fresh.Status != types.InstanceStatusTerminating {
					return store.ErrSkipUpdate
				}
				fresh.Status = types.InstanceStatusRunning
				fresh.StatusMessage = "Stop failed; still running"
				fresh.UpdatedAt = time.Now()
				return nil
			}); rerr == nil {
				instance.Status = types.InstanceStatusRunning
				c.republishServiceByInstance(ctx, instance)
				c.republishLocalInstances(ctx)
			}
		}
		return fmt.Errorf("failed to stop instance: %w", err)
	}

	// Update instance status to stopped — write ONLY the status fields atomically
	// on the fresh record so a concurrent health promotion / status write is not
	// clobbered (RFC #129 Phase 1c).
	originalStatus := instance.Status
	now := time.Now()
	var fresh types.Instance
	if err := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, &fresh, func() error {
		fresh.Status = types.InstanceStatusStopped
		fresh.StatusMessage = "Stopped by user"
		fresh.UpdatedAt = now
		return nil
	}); err != nil {
		c.logger.Error("Failed to update instance status",
			log.Str("instance", instance.ID),
			log.Str("from", string(originalStatus)),
			log.Str("to", string(types.InstanceStatusStopped)),
			log.Err(err))
		return fmt.Errorf("failed to update instance status: %w", err)
	}
	// Reflect on the caller's copy for consistency.
	instance.Status = types.InstanceStatusStopped
	instance.StatusMessage = "Stopped by user"
	instance.UpdatedAt = now

	c.logger.Info("Instance stopped successfully",
		log.Str("instance", instance.ID))
	c.republishServiceByInstance(ctx, instance)
	c.republishLocalInstances(ctx)
	return nil
}

// DeleteInstance marks an instance for deletion and cleans up runner resources
func (c *InstanceController) DeleteInstance(ctx context.Context, instance *types.Instance) error {
	c.logger.Info("Marking instance for deletion",
		log.Str("instance", instance.ID),
		log.Str("namespace", instance.Namespace),
		log.Str("service", instance.ServiceName))

	// Whether traffic could have been routed here: only Running instances
	// are ever published to the dataplane (republishService filters on
	// Status), so anything else was already absent from the endpoint set.
	// Captured before the Terminating flip below overwrites it. Batch
	// teardowns (WithdrawServiceInstances) pre-flip to Terminating and take
	// one shared drain, which makes this false and skips the per-instance
	// wait here.
	wasServing := instance.Status == types.InstanceStatusRunning

	// Flip to Terminating immediately so `rune get instances` shows
	// the truth ("this is being torn down") instead of Running during
	// the runner.Stop graceful-shutdown window (up to 10s here).
	// Best-effort; a store error doesn't block the teardown — the
	// final Status=Deleted write below is the authoritative one. Only
	// flip from non-terminal states (don't resurrect a Failed/Stalled
	// tombstone into Terminating).
	if instance.Status != types.InstanceStatusDeleted &&
		instance.Status != types.InstanceStatusFailed &&
		instance.Status != types.InstanceStatusStalled {
		instance.Status = types.InstanceStatusTerminating
		instance.StatusMessage = "Stopping and removing container"
		instance.UpdatedAt = time.Now()
		if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
			c.logger.Warn("Failed to mark instance Terminating before teardown",
				log.Str("instance", instance.ID),
				log.Err(err))
		}
	}

	// Withdraw from the dataplane BEFORE stopping the container (RUNE-042
	// §4, Phase 0). The Terminating flip above already makes this instance
	// ineligible for the endpoint set — publish that fact while the
	// container can still serve, then give in-flight requests the drain
	// window. The old order (stop first, withdraw at the end) handed new
	// connections to a dying container on every teardown; the republish at
	// the end of this function stays as an idempotent backstop.
	var owningService *types.Service
	if instance.ServiceName != "" {
		var svc types.Service
		if err := c.store.Get(ctx, types.ResourceTypeService, instance.Namespace, instance.ServiceName, &svc); err == nil {
			owningService = &svc
		}
	}
	if wasServing && owningService != nil {
		c.republishService(ctx, owningService)
		c.republishLocalInstances(ctx)
	}
	c.drainAfterWithdraw(ctx, owningService, wasServing, instance.ID)

	// Snapshot the container's stdout/stderr before we tear it down,
	// so `rune logs <id>` and the service-level tombstone fallback
	// can serve them after the container is gone. Best-effort; no-op
	// for instances that never had a container.
	c.snapshotInstanceLogs(ctx, instance)

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

// RestartInstance restarts an instance with respect to the service's restart policy
func (c *InstanceController) RestartInstance(ctx context.Context, instance *types.Instance, reason InstanceRestartReason) error {
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
		if serviceBeingDeleted(&service) {
			c.logger.Info("Skipping restart: service is being deleted",
				log.Str("instance", currentInstance.ID), log.Str("service", service.Name))
			return nil
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

	// Never resurrect an instance for a service that's being torn down
	// (RFC #129 Phase 4): the reconcileDeletion cascade is removing these
	// instances, and a health-triggered replacement here would race it and
	// leave an orphan the (now-retired) store-orphan sweep used to catch.
	if serviceBeingDeleted(&service) {
		c.logger.Info("Skipping restart: service is being deleted",
			log.Str("instance", instance.ID), log.Str("service", service.Name))
		return nil
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

	// Withdraw from the dataplane BEFORE stopping (RUNE-042 §4/§6.3): the
	// liveness-restart path had the same stop-before-withdraw defect as
	// DeleteInstance — new connections kept landing on the container being
	// restarted. Flip to Terminating (the tombstone write below moves it on
	// to Failed), publish the shrunken endpoint set, and give in-flight
	// requests the drain window.
	wasServing := currentInstance.Status == types.InstanceStatusRunning
	if wasServing {
		var fresh types.Instance
		if err := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, &fresh, func() error {
			if fresh.Status != types.InstanceStatusRunning {
				return store.ErrSkipUpdate
			}
			fresh.Status = types.InstanceStatusTerminating
			fresh.StatusMessage = "Restarting; draining connections"
			fresh.UpdatedAt = time.Now()
			return nil
		}, store.WithHealthController()); err != nil {
			c.logger.Warn("Failed to mark instance Terminating before restart",
				log.Str("instance", instance.ID),
				log.Err(err))
		}
		c.republishService(ctx, &service)
		c.republishLocalInstances(ctx)
	}
	c.drainAfterWithdraw(ctx, &service, wasServing, instance.ID)

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
	replacement, err := c.CreateInstance(ctx, &service, instance.Name, instance.Ordinal)
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
func (c *InstanceController) collectRunningInstances(ctx context.Context) (map[string]*RunningInstance, error) {
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
