package orchestrator

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/controllers"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// Orchestrator interface - main entry point (simplified)
type Orchestrator interface {
	// Lifecycle
	Start(ctx context.Context) error
	Stop() error

	// Service operations
	CreateService(ctx context.Context, service *types.Service) error
	UpdateService(ctx context.Context, service *types.Service) error
	DeleteService(ctx context.Context, request *types.DeletionRequest) (*types.DeletionResponse, error)
	GetService(ctx context.Context, namespace, name string) (*types.Service, error)
	ListServices(ctx context.Context, namespace string) ([]*types.Service, error)

	// Status and monitoring
	GetServiceStatus(ctx context.Context, namespace, name string) (*types.ServiceStatusInfo, error)
	GetInstanceStatus(ctx context.Context, namespace, instanceID string) (*types.InstanceStatusInfo, error)

	// Logs
	GetServiceLogs(ctx context.Context, namespace, name string, opts types.LogOptions) (io.ReadCloser, error)
	GetInstanceLogs(ctx context.Context, namespace, instanceID string, opts types.LogOptions) (io.ReadCloser, error)

	// Execution
	ExecInService(ctx context.Context, namespace, serviceName string, options types.ExecOptions) (types.ExecStream, error)
	ExecInInstance(ctx context.Context, namespace, instanceID string, options types.ExecOptions) (types.ExecStream, error)

	// Lifecycle operations
	GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error)
	RestartService(ctx context.Context, namespace, serviceName string) error
	RestartInstance(ctx context.Context, namespace, instanceID string) error
	StopService(ctx context.Context, namespace, serviceName string) error
	StopInstance(ctx context.Context, namespace, instanceID string) error

	// Deletion operations
	GetDeletionStatus(ctx context.Context, namespace, name string) (*types.DeletionOperation, error)
	ListDeletionOperations(ctx context.Context, namespace string) ([]*types.DeletionOperation, error)

	// Watch operations
	WatchServices(ctx context.Context, namespace string) (<-chan store.WatchEvent, error)

	// Instance operations
	ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error)
	ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error)

	// Scaling operations
	CreateScalingOperation(ctx context.Context, service *types.Service, params types.ScalingOperationParams) error
	GetActiveScalingOperation(ctx context.Context, namespace, serviceName string) (*types.ScalingOperation, error)
}

// orchestrator implements the Orchestrator interface
type orchestrator struct {
	// Core dependencies
	store  store.Store
	logger log.Logger

	// Controllers
	serviceController  controllers.ServiceController
	instanceController controllers.InstanceController
	healthController   controllers.HealthController
	scalingController  controllers.ScalingController

	// Runner manager for executing commands
	runnerManager manager.IRunnerManager

	// Context for background operations
	ctx    context.Context
	cancel context.CancelFunc

	// WaitGroup for goroutines
	wg sync.WaitGroup

	// State tracking
	started bool
	mu      sync.RWMutex
}

// OrchestratorOptions contains configuration for creating an orchestrator
type OrchestratorOptions struct {
	Store         store.Store
	Logger        log.Logger
	RunnerManager manager.IRunnerManager
	WorkerCount   int
	EnableMetrics bool
}

// NewDefaultOrchestrator creates a new orchestrator with default options
func NewDefaultOrchestrator(store store.Store, logger log.Logger, runnerManager manager.IRunnerManager) (Orchestrator, error) {
	return NewOrchestrator(OrchestratorOptions{
		Store:         store,
		Logger:        logger,
		RunnerManager: runnerManager,
		WorkerCount:   10,
		EnableMetrics: true,
	})
}

// NewOrchestrator creates a new orchestrator
func NewOrchestrator(options OrchestratorOptions) (Orchestrator, error) {
	if options.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if options.Logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if options.RunnerManager == nil {
		return nil, fmt.Errorf("runner manager is required")
	}

	// Set defaults
	if options.WorkerCount <= 0 {
		options.WorkerCount = 10
	}

	// Create controllers first (needed for finalizer system)
	instanceController := controllers.NewInstanceController(
		options.Store,
		options.RunnerManager,
		options.Logger,
	)

	healthController := controllers.NewHealthController(
		options.Logger,
		options.Store,
		options.RunnerManager,
		instanceController,
	)

	scalingController := controllers.NewScalingController(
		options.Store,
		options.Logger,
	)

	serviceController, err := controllers.NewServiceController(
		options.Store,
		instanceController,
		healthController,
		scalingController,
		options.Logger,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create service controller: %w", err)
	}

	return &orchestrator{
		store:              options.Store,
		logger:             options.Logger.WithComponent("orchestrator"),
		serviceController:  serviceController,
		instanceController: instanceController,
		healthController:   healthController,
		scalingController:  scalingController,
		runnerManager:      options.RunnerManager,
	}, nil
}

// Start starts the orchestrator and all its components
func (o *orchestrator) Start(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.started {
		return fmt.Errorf("orchestrator is already started")
	}

	o.logger.Info("Starting orchestrator")

	// Create context for background operations
	o.ctx, o.cancel = context.WithCancel(ctx)

	// Start controllers
	if err := o.serviceController.Start(o.ctx); err != nil {
		return fmt.Errorf("failed to start service controller: %w", err)
	}

	// Start health controller
	if err := o.healthController.Start(o.ctx); err != nil {
		return fmt.Errorf("failed to start health controller: %w", err)
	}

	// Start scaling controller
	if err := o.scalingController.Start(o.ctx); err != nil {
		return fmt.Errorf("failed to start scaling controller: %w", err)
	}

	o.started = true
	o.logger.Info("Orchestrator started successfully")
	return nil
}

// Stop stops the orchestrator and all its components
func (o *orchestrator) Stop() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.started {
		return nil
	}

	o.logger.Info("Stopping orchestrator")

	// Cancel context to stop all background operations
	if o.cancel != nil {
		o.cancel()
	}

	// Stop controllers
	if err := o.serviceController.Stop(); err != nil {
		o.logger.Error("Failed to stop service controller", log.Err(err))
	}

	// Stop health controller
	if err := o.healthController.Stop(); err != nil {
		o.logger.Error("Failed to stop health controller", log.Err(err))
	}

	// Stop scaling controller
	o.scalingController.Stop()

	// Wait for all goroutines to finish
	o.wg.Wait()

	o.started = false
	o.logger.Info("Orchestrator stopped successfully")
	return nil
}

// Service operations

func (o *orchestrator) CreateService(ctx context.Context, service *types.Service) error {
	o.logger.Info("Creating service",
		log.Str("name", service.Name),
		log.Str("namespace", service.Namespace))

	// Store the service - service controller watcher will pick this up
	if err := o.store.Create(ctx, types.ResourceTypeService, service.Namespace, service.Name, service); err != nil {
		return fmt.Errorf("failed to create service in store: %w", err)
	}

	o.logger.Info("Service created successfully",
		log.Str("name", service.Name),
		log.Str("namespace", service.Namespace))
	return nil
}

func (o *orchestrator) UpdateService(ctx context.Context, service *types.Service) error {
	o.logger.Info("Updating service",
		log.Str("name", service.Name),
		log.Str("namespace", service.Namespace))

	// Update the service in store - service controller watcher will pick this up
	if err := o.store.Update(ctx, types.ResourceTypeService, service.Namespace, service.Name, service); err != nil {
		return fmt.Errorf("failed to update service in store: %w", err)
	}

	o.logger.Info("Service updated successfully",
		log.Str("name", service.Name),
		log.Str("namespace", service.Namespace))
	return nil
}

func (o *orchestrator) DeleteService(ctx context.Context, request *types.DeletionRequest) (*types.DeletionResponse, error) {
	return o.serviceController.DeleteService(ctx, request)
}

func (o *orchestrator) GetService(ctx context.Context, namespace, name string) (*types.Service, error) {
	var service types.Service
	if err := o.store.Get(ctx, types.ResourceTypeService, namespace, name, &service); err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}
	return &service, nil
}

func (o *orchestrator) ListServices(ctx context.Context, namespace string) ([]*types.Service, error) {
	var services []types.Service
	if err := o.store.List(ctx, types.ResourceTypeService, namespace, &services); err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	// Convert to pointers
	result := make([]*types.Service, len(services))
	for i := range services {
		result[i] = &services[i]
	}

	return result, nil
}

// Status and monitoring operations

func (o *orchestrator) GetServiceStatus(ctx context.Context, namespace, name string) (*types.ServiceStatusInfo, error) {
	return o.serviceController.GetServiceStatus(ctx, namespace, name)
}

func (o *orchestrator) GetInstanceStatus(ctx context.Context, namespace, instanceID string) (*types.InstanceStatusInfo, error) {
	// Get the instance from store
	instance, err := o.store.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}

	// Create status info
	statusInfo := &types.InstanceStatusInfo{
		Status:        instance.Status,
		StatusMessage: instance.StatusMessage,
		InstanceID:    instance.ID,
		NodeID:        instance.NodeID,
		CreatedAt:     instance.CreatedAt,
		UpdatedAt:     instance.UpdatedAt,
	}

	return statusInfo, nil
}

// Log operations

func (o *orchestrator) GetServiceLogs(ctx context.Context, namespace, name string, opts types.LogOptions) (io.ReadCloser, error) {
	return o.serviceController.GetServiceLogs(ctx, namespace, name, opts)
}

func (o *orchestrator) GetInstanceLogs(ctx context.Context, namespace, instanceID string, opts types.LogOptions) (io.ReadCloser, error) {
	// Get the instance from store
	instance, err := o.store.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}

	// Delegate to instance controller for log retrieval
	return o.instanceController.GetInstanceLogs(ctx, instance, opts)
}

// Execution operations

func (o *orchestrator) ExecInService(ctx context.Context, namespace, serviceName string, options types.ExecOptions) (types.ExecStream, error) {
	return o.serviceController.ExecInService(ctx, namespace, serviceName, options)
}

func (o *orchestrator) ExecInInstance(ctx context.Context, namespace, instanceID string, options types.ExecOptions) (types.ExecStream, error) {
	// Get the instance from store
	instance, err := o.store.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}

	// Delegate to instance controller for execution
	return o.instanceController.Exec(ctx, instance, options)
}

// Lifecycle operations

func (o *orchestrator) GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	return o.instanceController.GetInstanceByID(ctx, namespace, instanceID)
}

func (o *orchestrator) RestartService(ctx context.Context, namespace, serviceName string) error {
	return o.serviceController.RestartService(ctx, namespace, serviceName)
}

func (o *orchestrator) RestartInstance(ctx context.Context, namespace, instanceID string) error {
	// Get the instance from store
	instance, err := o.store.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}

	// Delegate to instance controller for restart
	return o.instanceController.RestartInstance(ctx, instance, controllers.InstanceRestartReasonManual)
}

func (o *orchestrator) StopService(ctx context.Context, namespace, serviceName string) error {
	return o.serviceController.StopService(ctx, namespace, serviceName)
}

func (o *orchestrator) StopInstance(ctx context.Context, namespace, instanceID string) error {
	// Get the instance from store
	instance, err := o.store.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}

	// Delegate to instance controller for stop
	return o.instanceController.StopInstance(ctx, instance)
}

// Deletion operations

func (o *orchestrator) GetDeletionStatus(ctx context.Context, namespace, name string) (*types.DeletionOperation, error) {
	return o.serviceController.GetDeletionStatus(ctx, namespace, name)
}

func (o *orchestrator) ListDeletionOperations(ctx context.Context, namespace string) ([]*types.DeletionOperation, error) {
	var deletionOps []types.DeletionOperation
	err := o.store.List(ctx, types.ResourceTypeDeletionOperation, namespace, &deletionOps)
	if err != nil {
		return nil, fmt.Errorf("failed to list deletion operations: %w", err)
	}

	// Convert to pointers
	result := make([]*types.DeletionOperation, len(deletionOps))
	for i := range deletionOps {
		result[i] = &deletionOps[i]
	}

	return result, nil
}

func (o *orchestrator) WatchServices(ctx context.Context, namespace string) (<-chan store.WatchEvent, error) {
	return o.store.Watch(ctx, types.ResourceTypeService, namespace)
}

func (o *orchestrator) ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	return o.instanceController.ListInstances(ctx, namespace)
}

func (o *orchestrator) ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	return o.instanceController.ListRunningInstances(ctx, namespace)
}

// Scaling operations

func (o *orchestrator) CreateScalingOperation(ctx context.Context, service *types.Service, params types.ScalingOperationParams) error {
	return o.scalingController.CreateScalingOperation(ctx, service, params)
}

func (o *orchestrator) GetActiveScalingOperation(ctx context.Context, namespace, serviceName string) (*types.ScalingOperation, error) {
	return o.scalingController.GetActiveOperation(ctx, namespace, serviceName)
}
