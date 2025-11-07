package controllers

import (
	"context"
	"fmt"
	"io"
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

	// Store the service generation in instance metadata
	instance.Metadata.ServiceGeneration = service.Metadata.Generation

	// Propagate ports and expose spec for runner use and later status
	if len(service.Ports) > 0 {
		instance.Metadata.Ports = append(instance.Metadata.Ports, service.Ports...)
	}
	if service.Expose != nil {
		instance.Metadata.Expose = service.Expose
	}
	c.logger.Debug("Storing service generation in instance",
		log.Str("instance", instanceName),
		log.Int64("generation", service.Metadata.Generation))

	// Save instance to store
	if err := c.store.Create(ctx, types.ResourceTypeInstance, service.Namespace, instance.ID, instance); err != nil {
		return nil, fmt.Errorf("failed to create instance in store: %w", err)
	}

	// Create instance based on runtime
	serviceRunner, err := c.runnerManager.GetServiceRunner(service)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for service: %w", err)
	}

	// Set the runner type for the instance
	instance.Runner = serviceRunner.Type()

	// Build environment variables and interpolate secret:/config: values
	envVars, err := c.prepareEnvVars(ctx, service, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare environment variables: %w", err)
	}
	c.logger.Debug("Prepared environment variables",
		log.Str("instance", instanceName),
		log.Int("env_var_count", len(envVars)))

	// Set environment variables in the instance
	instance.Environment = envVars

	// Resolve secret and config mounts for this instance
	if err := c.resolveMounts(ctx, service, instance); err != nil {
		return nil, fmt.Errorf("failed to resolve secret and config mounts: %w", err)
	}

	// Store original image for future compatibility checks
	if service.Image != "" {
		if instance.Metadata == nil {
			instance.Metadata = &types.InstanceMetadata{}
		}
		instance.Metadata.Image = service.Image
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
		instance.Status = types.InstanceStatusFailed
		if updateErr := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); updateErr != nil {
			c.logger.Error("Failed to update instance status after create failure",
				log.Str("instance", instance.ID),
				log.Err(updateErr))
		}
		return nil, fmt.Errorf("failed to create instance: %w", err)
	}

	// Start the instance
	if err := serviceRunner.Start(ctx, instance); err != nil {
		instance.Status = types.InstanceStatusFailed
		if updateErr := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); updateErr != nil {
			c.logger.Error("Failed to update instance status after start failure",
				log.Str("instance", instance.ID),
				log.Err(updateErr))
		}
		return nil, fmt.Errorf("failed to start instance: %w", err)
	}

	// Update instance with running status
	instance.Status = types.InstanceStatusRunning
	instance.StatusMessage = "Created successfully"
	if err := c.store.Update(ctx, types.ResourceTypeInstance, service.Namespace, instance.ID, instance); err != nil {
		c.logger.Error("Failed to update instance status",
			log.Str("instance", instance.ID),
			log.Err(err))
	}

	return instance, nil
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
	return nil
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

	// Update instance status to restarting
	instance.Status = types.InstanceStatusStarting
	instance.StatusMessage = fmt.Sprintf("Restarting due to: %s", reason)
	instance.UpdatedAt = time.Now()

	if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
		c.logger.Error("Failed to update instance status before restart",
			log.Str("instance", instance.ID),
			log.Err(err))
		// Continue anyway
	}

	// Stop the instance first
	stopTimeout := 10 * time.Second
	if err := runner.Stop(ctx, instance, stopTimeout); err != nil {
		c.logger.Warn("Failed to stop instance gracefully, will force restart",
			log.Str("instance", instance.ID),
			log.Err(err))
	}

	// Always remove the container to ensure clean restart
	if err := runner.Remove(ctx, instance, true); err != nil {
		c.logger.Error("Failed to remove instance during restart",
			log.Str("instance", instance.ID),
			log.Err(err))
		// Continue anyway, but this might cause container name conflicts
	}

	// Clear the container ID to ensure a fresh container is created
	instance.ContainerID = ""

	// Create the instance again
	if err := runner.Create(ctx, instance); err != nil {
		instance.Status = types.InstanceStatusFailed
		instance.StatusMessage = fmt.Sprintf("Failed to recreate: %v", err)
		if updateErr := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); updateErr != nil {
			c.logger.Error("Failed to update instance status after create failure",
				log.Str("instance", instance.ID),
				log.Err(updateErr))
		}
		return fmt.Errorf("failed to recreate instance: %w", err)
	}

	// Start the instance
	if err := runner.Start(ctx, instance); err != nil {
		instance.Status = types.InstanceStatusFailed
		instance.StatusMessage = fmt.Sprintf("Failed to start: %v", err)
		if updateErr := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); updateErr != nil {
			c.logger.Error("Failed to update instance status after start failure",
				log.Str("instance", instance.ID),
				log.Err(updateErr))
		}
		return fmt.Errorf("failed to start instance: %w", err)
	}

	// Increment restart count in instance metadata
	if instance.Metadata == nil {
		instance.Metadata = &types.InstanceMetadata{}
	}
	instance.Metadata.RestartCount++

	// Update instance with running status and restart count
	instance.Status = types.InstanceStatusRunning
	instance.StatusMessage = "Restarted successfully"
	instance.UpdatedAt = time.Now()

	if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
		c.logger.Error("Failed to update instance status after restart",
			log.Str("instance", instance.ID),
			log.Err(err))
	}

	c.logger.Info("Instance restarted successfully",
		log.Str("instance", instance.ID),
		log.Int("restart_count", instance.Metadata.RestartCount))

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

	return nil
}
