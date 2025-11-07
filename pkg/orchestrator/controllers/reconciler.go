package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// The amount of time to keep deleted instances before removing them from store
const deletedInstanceRetentionTime = 10 * time.Minute

// The amount of time to wait before running garbage collection
const garbageCollectionInterval = 1 * time.Minute

// reconciler is responsible for ensuring the actual state of instances
// matches the desired state defined in the services
type reconciler struct {
	store              store.Store
	instanceController InstanceController
	healthController   HealthController
	logger             log.Logger
	reconcileInterval  time.Duration
	mu                 sync.Mutex
	isRunning          bool
	ctx                context.Context
	cancel             context.CancelFunc
	ticker             *time.Ticker
	wg                 sync.WaitGroup
}

// newReconciler creates a new reconciler.
func newReconciler(
	store store.Store,
	instanceController InstanceController,
	healthController HealthController,
	logger log.Logger,
) *reconciler {
	return &reconciler{
		store:              store,
		instanceController: instanceController,
		healthController:   healthController,
		logger:             logger.WithComponent("reconciler"),
		reconcileInterval:  30 * time.Second,
		mu:                 sync.Mutex{},
		wg:                 sync.WaitGroup{},
	}
}

// Start begins the periodic reconciliation loop
func (r *reconciler) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.isRunning {
		r.mu.Unlock()
		return nil // Already running, nothing to do
	}
	r.isRunning = true
	r.mu.Unlock()

	r.logger.Info("Starting reconciler")

	r.ctx, r.cancel = context.WithCancel(ctx)

	// Perform an initial reconciliation immediately
	if err := r.reconcileServices(r.ctx); err != nil {
		r.logger.Error("Initial reconciliation failed", log.Err(err))
		// Continue despite error as this will be retried by the ticker
	}

	// Start periodic reconciliation
	r.ticker = time.NewTicker(r.reconcileInterval)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		// Track the last time we ran garbage collection
		// Initially run after 5 minutes to ensure system is stable
		lastGC := time.Now().Add(-55 * time.Minute)

		for {
			select {
			case <-r.ctx.Done():
				return
			case <-r.ticker.C:
				r.logger.Debug("Running periodic reconciliation")

				// Reconcile all services
				if err := r.reconcileServices(r.ctx); err != nil {
					r.logger.Error("Reconciliation failed", log.Err(err))
				}

				// Clean up deleted services
				if err := r.handleDeletedServices(r.ctx); err != nil {
					r.logger.Error("Failed to clean up deleted services", log.Err(err))
				}

				// Run garbage collection roughly once per hour
				if time.Since(lastGC) > garbageCollectionInterval {
					if err := r.runGarbageCollection(r.ctx); err != nil {
						r.logger.Error("Garbage collection failed", log.Err(err))
					}
					lastGC = time.Now()
				}
			}
		}
	}()

	return nil
}

// Stop stops the reconciliation loop.
func (r *reconciler) Stop() {
	r.mu.Lock()
	if !r.isRunning {
		r.mu.Unlock()
		return // Not running, nothing to do
	}
	r.mu.Unlock()

	r.logger.Info("Stopping reconciler")

	// Cancel context to stop operations
	if r.cancel != nil {
		r.cancel()
	}

	// Stop the ticker
	if r.ticker != nil {
		r.ticker.Stop()
	}

	// Wait for goroutines to finish
	r.wg.Wait()

	// Mark as not running
	r.mu.Lock()
	r.isRunning = false
	r.mu.Unlock()

	r.logger.Info("Reconciler stopped")
}

// reconcileServices compares the desired state of services with the actual state
// and makes any necessary adjustments
func (r *reconciler) reconcileServices(ctx context.Context) error {
	r.logger.Debug("Starting service reconciliation")

	// Get desired state from store
	var services []types.Service
	if err := r.store.ListAll(ctx, types.ResourceTypeService, &services); err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	// Collect all orphaned instances across all services
	var allOrphanedInstances []*types.Instance

	// Reconcile each service's desired state with actual state
	for _, service := range services {
		if err := r.reconcileService(ctx, &service); err != nil {
			r.logger.Error("Failed to reconcile service",
				log.Str("service", service.Name),
				log.Str("namespace", service.Namespace),
				log.Err(err))
			// Continue with other services even if one fails
		}

		// Collect orphaned instances for this service
		instanceData, err := r.getServiceInstances(ctx, &service)
		if err != nil {
			r.logger.Error("Failed to get service instances for orphaned detection",
				log.Str("service", service.Name),
				log.Err(err))
			continue
		}
		allOrphanedInstances = append(allOrphanedInstances, instanceData.OrphanedInstances...)
	}

	r.logger.Debug("Completed reconciliation for services", log.Int("count", len(services)))

	// Handle orphaned instances (running but not in desired state)
	if err := r.cleanUpOrphanedInstances(ctx, allOrphanedInstances); err != nil {
		r.logger.Error("Failed to clean up orphaned instances", log.Err(err))
	}

	r.logger.Debug("Service reconciliation completed")
	return nil
}

func (r *reconciler) cleanUpOrphanedInstances(ctx context.Context, orphanedInstances []*types.Instance) error {
	for _, instance := range orphanedInstances {
		if instance != nil {
			r.logger.Info("Cleaning up orphaned instance",
				log.Str("instance", instance.ID),
				log.Str("service", instance.ServiceID))
			if err := r.instanceController.DeleteInstance(ctx, instance); err != nil {
				r.logger.Error("Failed to clean up orphaned instance",
					log.Str("instance", instance.ID),
					log.Err(err))
				return err
			}

		}
	}
	return nil
}

// reconcileService ensures that a single service's instances match the desired state
func (r *reconciler) reconcileService(ctx context.Context, service *types.Service) error {
	r.logger.Debug("Reconciling service",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace))

	// Skip reconciliation if the service is marked as deleted
	if service.Status == types.ServiceStatusDeleted {
		r.logger.Debug("Skipping reconciliation for deleted service",
			log.Str("service", service.Name),
			log.Str("namespace", service.Namespace))
		return nil
	}

	// Scale down if needed
	if err := r.scaleDownService(ctx, service); err != nil {
		r.logger.Error("Error scaling down service",
			log.Str("service", service.Name),
			log.Err(err))
		// Continue with the rest of reconciliation
	}

	// Ensure we have the right number of instances and they're up to date
	if err := r.ensureServiceInstances(ctx, service); err != nil {
		r.logger.Error("Error ensuring service instances",
			log.Str("service", service.Name),
			log.Err(err))
		// Continue with the rest of reconciliation
	}

	// Update service status based on the latest instance data
	if err := r.updateServiceStatus(ctx, service); err != nil {
		r.logger.Error("Failed to update service status",
			log.Str("service", service.Name),
			log.Err(err))
	}

	return nil
}

// reconcileSingleService reconciles a single service with running instances collection
// This is used by the service controller for immediate reconciliation on service events
func (r *reconciler) reconcileSingleService(ctx context.Context, service *types.Service) error {
	r.logger.Debug("Reconciling single service",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace))

	// Reconcile the service using the existing logic
	return r.reconcileService(ctx, service)
}

// getServiceInstances retrieves and filters instances for a specific service
// Marks running instances that belong to this service as not orphaned
func (r *reconciler) getServiceInstances(ctx context.Context, service *types.Service) (*ServiceInstanceData, error) {
	// Get running instances from all runners
	runningInstances, err := r.instanceController.collectRunningInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to collect running instances: %w", err)
	}

	// Get existing instances for this service from store
	var storeInstances []types.Instance
	err = r.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &storeInstances)
	if err != nil {
		return nil, fmt.Errorf("failed to get instances for service: %w", err)
	}

	r.logger.Debug("Retrieved instances for service",
		log.Str("service", service.Name),
		log.Int("count", len(storeInstances)))

	// Filter instances for this service, skipping deleted ones
	var serviceInstances []types.Instance
	var orphanedInstances []*types.Instance

	// Track which running instances belong to this service
	serviceRunningInstances := make(map[string]bool)

	for _, instance := range storeInstances {
		if instance.ServiceName == service.Name && instance.Status != types.InstanceStatusDeleted {
			serviceInstances = append(serviceInstances, instance)
			serviceRunningInstances[instance.ID] = true
		}
	}

	// Check for orphaned instances (running but not in store for this service)
	for instanceID, runningInst := range runningInstances {
		if !serviceRunningInstances[instanceID] {
			// This instance is running but not in our service's store instances
			// Check if it belongs to this service
			if runningInst.Instance != nil && runningInst.Instance.ServiceName == service.Name {
				orphanedInstances = append(orphanedInstances, runningInst.Instance)
			}
		}
	}

	return &ServiceInstanceData{
		Instances:         serviceInstances,
		OrphanedInstances: orphanedInstances,
	}, nil
}

// scaleDownService removes excess instances when the desired scale is lower than current
func (r *reconciler) scaleDownService(ctx context.Context, service *types.Service) error {
	// Get existing instances for this service
	instanceData, err := r.getServiceInstances(ctx, service)
	if err != nil {
		return err
	}

	if len(instanceData.Instances) <= service.Scale {
		return nil // No scaling down needed
	}

	r.logger.Info("Scaling down service",
		log.Str("service", service.Name),
		log.Int("current", len(instanceData.Instances)),
		log.Int("desired", service.Scale))

	// Sort instances by creation time (newest first). If zero timestamps in tests, sort by name for stability
	sort.Slice(instanceData.Instances, func(i, j int) bool {
		a := instanceData.Instances[i]
		b := instanceData.Instances[j]
		if !a.CreatedAt.IsZero() || !b.CreatedAt.IsZero() {
			return a.CreatedAt.After(b.CreatedAt)
		}
		// Fallback for tests where CreatedAt may be zero: sort by name ascending
		return a.Name < b.Name
	})

	// For now we'll use a simple approach removing from the end
	for i := service.Scale; i < len(instanceData.Instances); i++ {
		instance := instanceData.Instances[i]

		r.logger.Info("Removing excess instance",
			log.Str("service", service.Name),
			log.Str("instance", instance.ID))

		// Always remove from health monitoring
		// (regardless of current service.Health configuration)
		r.healthController.RemoveInstance(instance.ID)

		if err := r.instanceController.DeleteInstance(ctx, &instance); err != nil {
			r.logger.Error("Failed to remove excess instance",
				log.Str("instance", instance.ID),
				log.Err(err))
			// Continue with other instances even if one fails
		}
	}

	return nil
}

// ensureServiceInstances makes sure we have the right number of instances and they're up to date
func (r *reconciler) ensureServiceInstances(ctx context.Context, service *types.Service) error {
	// If the service declares dependencies, gate instance creation until deps are ready
	if len(service.Dependencies) > 0 {
		ready, err := r.dependenciesReady(ctx, service)
		if err != nil {
			r.logger.Error("Dependency readiness check failed",
				log.Str("service", service.Name),
				log.Err(err))
			// Be safe: do not proceed with instance creation on error
			return nil
		}
		if !ready {
			r.logger.Info("Delaying instance creation; dependencies not ready",
				log.Str("service", service.Name),
				log.Str("namespace", service.Namespace))
			return nil
		}
	}

	// Get existing instances for this service
	instanceData, err := r.getServiceInstances(ctx, service)
	if err != nil {
		return err
	}
	r.logger.Debug("Ensuring service instances",
		log.Str("service", service.Name),
		log.Int("desired", service.Scale),
		log.Int("current", len(instanceData.Instances)))

	for i := 0; i < service.Scale; i++ {
		// Generate instance name
		instanceName := generateInstanceName(service, i)

		// Check if this instance already exists and is compatible
		var existingInstance *types.Instance
		for j := range instanceData.Instances {
			if instanceData.Instances[j].Name == instanceName {
				// Use the existing compatibility check function
				isCompatible, reason := r.instanceController.isInstanceCompatibleWithService(ctx, &instanceData.Instances[j], service)
				if isCompatible {
					existingInstance = &instanceData.Instances[j]
					break
				} else {
					r.logger.Info("Instance incompatible, will recreate",
						log.Str("service", service.Name),
						log.Str("instance", instanceName),
						log.Str("reason", reason))
					// Always remove the old instance from health monitoring
					// (regardless of current service.Health configuration)
					r.healthController.RemoveInstance(instanceData.Instances[j].ID)
					// Delete the old instance
					if err := r.instanceController.DeleteInstance(ctx, &instanceData.Instances[j]); err != nil {
						r.logger.Error("Failed to delete old instance during recreation",
							log.Str("instance", instanceData.Instances[j].ID),
							log.Err(err))
					}
				}
			}
		}

		if existingInstance != nil {
			// Update existing instance
			if err := r.reconcileExistingInstance(ctx, service, existingInstance); err != nil {
				r.logger.Error("Failed to reconcile existing instance",
					log.Str("service", service.Name),
					log.Str("instance", instanceName),
					log.Err(err))
				// Continue with other instances
			}
			continue
		}

		r.logger.Info("creating new instance", log.Json("instanceName", instanceName))
		// Create a new instance
		if err := r.createNewInstance(ctx, service, instanceName); err != nil {
			r.logger.Error("Failed to create new instance",
				log.Str("service", service.Name),
				log.Str("instance", instanceName),
				log.Err(err))
			// Continue with other instances
		}
	}

	return nil
}

// dependenciesReady evaluates whether all declared dependencies for a service are ready.
// Readiness definition (MVP):
// - If dependency is not a service, it's ready
// - If dependency is a service and it defines readiness probe: at least one instance is Running and readiness=true
// - Else: at least one instance is Running
func (r *reconciler) dependenciesReady(ctx context.Context, service *types.Service) (bool, error) {
	for _, dep := range service.Dependencies {
		// Fetch dependency service
		dependencyResource, err := r.fetchDependencyResource(ctx, &dep, service)
		if err != nil {
			return false, err
		}

		// If dependency is not a service, it's ready
		depService, ok := dependencyResource.(types.Service)
		if !ok {
			continue
		}

		// List instances for dependency
		instances, err := r.listInstancesForService(ctx, depService.Namespace, dep.Service)
		if err != nil {
			return false, fmt.Errorf("failed to list instances for dependency %s/%s: %w", depService.Namespace, dep.Service, err)
		}

		// Evaluate readiness
		readyFound := false
		hasReadinessProbe := depService.Health != nil && depService.Health.Readiness != nil
		for _, inst := range instances {
			if inst.Status != types.InstanceStatusRunning {
				continue
			}
			if !hasReadinessProbe {
				// Running instance is sufficient
				readyFound = true
				break
			}
			// Check readiness via health controller
			status, err := r.healthController.GetHealthStatus(ctx, inst.ID)
			if err != nil {
				// On error, treat as not ready; continue
				continue
			}
			if status != nil && status.Readiness {
				readyFound = true
				break
			}
		}
		if !readyFound {
			return false, nil
		}
	}
	return true, nil
}

func (r *reconciler) fetchDependencyResource(ctx context.Context, dep *types.DependencyRef, service *types.Service) (interface{}, error) {
	var dependencyResource interface{}
	depNS := dep.Namespace
	if depNS == "" {
		depNS = service.Namespace
	}

	depResourceType := dep.GetDependencyResourceType()
	depResourceName := dep.GetDependencyResourceName()

	// For dependencies, fetch into concrete types so the store can unmarshal correctly
	switch depResourceType {
	case types.ResourceTypeService:
		var svc types.Service
		// Use explicit service name if provided, else fall back to computed name
		name := dep.Service
		if name == "" {
			name = depResourceName
		}
		if err := r.store.Get(ctx, types.ResourceTypeService, depNS, name, &svc); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, name, err)
		}
		return svc, nil
	case types.ResourceTypeConfigmap:
		var cfg types.Configmap
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &cfg); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return cfg, nil
	case types.ResourceTypeSecret:
		var sec types.Secret
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &sec); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return sec, nil
	default:
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &dependencyResource); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return dependencyResource, nil
	}
}

// reconcileExistingInstance updates an existing instance, recreating it if necessary
func (r *reconciler) reconcileExistingInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	r.logger.Debug("Reconciling existing instance",
		log.Str("service", service.Name),
		log.Str("instance", instance.ID))

	// Try to update the instance in-place
	if err := r.instanceController.UpdateInstance(ctx, service, instance); err != nil {
		// Check if the error indicates that recreation is needed
		if r.isRecreationRequired(err) {
			return r.recreateInstance(ctx, service, instance)
		}
		// Some other update error occurred
		return fmt.Errorf("failed to update instance: %w", err)
	}

	// Add instance to health monitoring if needed
	if service.Health != nil {
		if err := r.healthController.AddInstance(service, instance); err != nil {
			r.logger.Error("Failed to add instance to health monitoring",
				log.Str("instance", instance.ID),
				log.Err(err))
		}
		r.logger.Info("Added instance to health monitoring",
			log.Str("instance", instance.Name),
			log.Str("service", service.Name))
	}

	return nil
}

// recreateInstance handles recreation of an instance that cannot be updated in-place
func (r *reconciler) recreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	instanceName := instance.ID
	r.logger.Info("Instance requires recreation",
		log.Str("service", service.Name),
		log.Str("instance", instanceName))

	// First remove from health monitoring if applicable
	if service.Health != nil {
		r.healthController.RemoveInstance(instance.ID)
	}

	// Recreate the instance
	r.logger.Info("Recreating instance",
		log.Str("service", service.Name),
		log.Str("instance", instanceName))

	newInstance, err := r.instanceController.RecreateInstance(ctx, service, instance)
	if err != nil {
		return fmt.Errorf("failed to recreate instance: %w", err)
	}

	// Add the new instance to health monitoring if needed
	if service.Health != nil {
		if err := r.healthController.AddInstance(service, newInstance); err != nil {
			r.logger.Error("Failed to add recreated instance to health monitoring",
				log.Str("instance", instanceName),
				log.Err(err))
		}
	}

	return nil
}

// createNewInstance creates a new instance for a service
func (r *reconciler) createNewInstance(ctx context.Context, service *types.Service, instanceName string) error {
	r.logger.Info("Creating instance to achieve desired scale",
		log.Str("service", service.Name),
		log.Str("instance", instanceName))

	newInstance, err := r.instanceController.CreateInstance(ctx, service, instanceName)
	if err != nil {
		return fmt.Errorf("failed to create instance: %w", err)
	}

	// Add the instance to health monitoring if applicable
	if service.Health != nil {
		if err := r.healthController.AddInstance(service, newInstance); err != nil {
			r.logger.Error("Failed to add instance to health monitoring",
				log.Str("instance", instanceName),
				log.Err(err))
			// Continue anyway
		}
	}

	return nil
}

// updateServiceStatus updates a service's status based on its instances
func (r *reconciler) updateServiceStatus(ctx context.Context, service *types.Service) error {
	// Don't change the status if service is already marked as deleted
	if service.Status == types.ServiceStatusDeleted {
		return nil
	}

	// Fetch the latest instance data directly from the store
	instanceData, err := r.getServiceInstances(ctx, service)
	if err != nil {
		return fmt.Errorf("failed to get latest instances for status update: %w", err)
	}

	var newStatus types.ServiceStatus

	if len(instanceData.Instances) == 0 {
		// No instances yet
		newStatus = types.ServiceStatusPending
	} else {
		// Count instances in each state
		pending := 0
		running := 0
		failed := 0

		for _, instance := range instanceData.Instances {
			switch instance.Status {
			case types.InstanceStatusPending, types.InstanceStatusCreated, types.InstanceStatusStarting:
				pending++
			case types.InstanceStatusRunning:
				running++
			case types.InstanceStatusFailed, types.InstanceStatusExited, types.InstanceStatusUnknown:
				failed++
			}
		}

		r.logger.Debug("Instance status counts",
			log.Str("service", service.Name),
			log.Int("pending", pending),
			log.Int("running", running),
			log.Int("failed", failed),
			log.Int("total", len(instanceData.Instances)))

		// Determine overall service status
		if failed > 0 {
			newStatus = types.ServiceStatusFailed
		} else if pending > 0 {
			newStatus = types.ServiceStatusDeploying
		} else if running == len(instanceData.Instances) {
			newStatus = types.ServiceStatusRunning
		} else {
			newStatus = types.ServiceStatusPending
		}
	}

	// Update service status if changed
	if service.Status != newStatus {
		r.logger.Info("Updating service status",
			log.Str("service", service.Name),
			log.Str("from", string(service.Status)),
			log.Str("to", string(newStatus)))

		service.Status = newStatus
		if err := r.store.Update(ctx, types.ResourceTypeService, service.Namespace, service.Name, service); err != nil {
			return fmt.Errorf("failed to update service status: %w", err)
		}
	}

	return nil
}

// ServiceInstanceData contains instances and orphaned instance information
type ServiceInstanceData struct {
	Instances         []types.Instance
	OrphanedInstances []*types.Instance // Actual orphaned instance objects
}

// RunningInstance represents an instance found running in a runner
type RunningInstance struct {
	Instance   *types.Instance
	IsOrphaned bool
	Runner     types.RunnerType
}

// handleDeletedServices cleans up services that are marked as deleted
func (r *reconciler) handleDeletedServices(ctx context.Context) error {
	// Get all services from store
	var services []types.Service
	err := r.store.ListAll(ctx, types.ResourceTypeService, &services)
	if err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	// Check for services marked as deleted
	for _, service := range services {
		// If service is marked as deleted, remove it from the store
		if service.Status == types.ServiceStatusDeleted {
			// Check if all instances have been cleaned up
			instances, err := r.listInstancesForService(ctx, service.Namespace, service.Name)
			if err != nil {
				r.logger.Error("Failed to list instances for deleted service",
					log.Str("name", service.Name),
					log.Str("namespace", service.Namespace),
					log.Err(err))
				continue
			}

			if len(instances) == 0 {
				// All instances are gone, we can remove the service from the store
				r.logger.Info("Removing deleted service from store",
					log.Str("name", service.Name),
					log.Str("namespace", service.Namespace))

				if err := r.store.Delete(ctx, types.ResourceTypeService, service.Namespace, service.Name); err != nil {
					r.logger.Error("Failed to remove deleted service from store",
						log.Str("name", service.Name),
						log.Str("namespace", service.Namespace),
						log.Err(err))
				}
			}
		}
	}

	return nil
}

// listInstancesForService lists all instances for a service
func (r *reconciler) listInstancesForService(ctx context.Context, namespace, serviceName string) ([]types.Instance, error) {
	// Get all instances
	var instances []types.Instance
	err := r.store.List(ctx, types.ResourceTypeInstance, namespace, &instances)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	// Filter instances for this service (by ServiceName)
	filtered := make([]types.Instance, 0, len(instances))
	for _, instance := range instances {
		if instance.ServiceName == serviceName {
			filtered = append(filtered, instance)
		}
	}

	return filtered, nil
}

// isRecreationRequired checks if an error from UpdateInstance indicates that
// the instance needs to be recreated rather than updated in-place
func (r *reconciler) isRecreationRequired(err error) bool {
	if err == nil {
		return false
	}

	// Check for the specific error message pattern from UpdateInstance
	return strings.Contains(err.Error(), "requires recreation") ||
		strings.Contains(err.Error(), "incompatible") ||
		strings.Contains(err.Error(), "cannot be updated in-place")
}

// runGarbageCollection removes instances that have been marked as deleted
// after a specified retention period has passed
func (r *reconciler) runGarbageCollection(ctx context.Context) error {
	r.logger.Debug("Running garbage collection")

	// Get all instances
	var instances []types.Instance
	err := r.store.ListAll(ctx, types.ResourceTypeInstance, &instances)
	if err != nil {
		return fmt.Errorf("failed to list instances for garbage collection: %w", err)
	}

	// Filter for deleted instances
	for _, instance := range instances {
		if instance.Status == types.InstanceStatusDeleted && instance.Metadata != nil {
			// Check if retention period has passed
			if instance.Metadata.DeletionTimestamp == nil {
				// No timestamp, keep it for now and log the issue
				r.logger.Warn("Found deleted instance without deletion timestamp",
					log.Str("instance", instance.ID),
					log.Str("namespace", instance.Namespace))
				continue
			}

			// Parse the timestamp
			deletedAt := *instance.Metadata.DeletionTimestamp

			// Check if retention period has passed
			if time.Since(deletedAt) > deletedInstanceRetentionTime {
				r.logger.Info("Garbage collecting deleted instance",
					log.Str("instance", instance.ID),
					log.Str("service", instance.ServiceName),
					log.Str("namespace", instance.Namespace),
					log.Str("deletedAt", deletedAt.Format(time.RFC3339)),
					log.Str("age", time.Since(deletedAt).String()))

				// Remove from store
				if err := r.store.Delete(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID); err != nil {
					r.logger.Error("Failed to garbage collect instance",
						log.Str("instance", instance.ID),
						log.Err(err))
				} else {
					r.logger.Info("Successfully garbage collected instance",
						log.Str("instance", instance.ID))
				}
			}
		}
	}

	return nil
}
