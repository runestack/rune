package orchestrator

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/health"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
	"github.com/runestack/rune/pkg/orchestrator/scaling"
	"github.com/runestack/rune/pkg/orchestrator/service"
	"github.com/runestack/rune/pkg/orchestrator/volume"
	"github.com/runestack/rune/pkg/orchestrator/wiring"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/storage/driverparams"
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
	// DebugInInstance spawns an ephemeral inspection sidecar from a
	// Failed instance's template (image, env, mounts) with the
	// entrypoint overridden to `sleep infinity`, then execs the user's
	// command inside it. Cleanup happens on ExecStream.Close.
	DebugInInstance(ctx context.Context, namespace, instanceID string, options types.ExecOptions) (types.ExecStream, error)

	// Port-forward (RUNE-122)
	DialInService(ctx context.Context, namespace, serviceName string, port uint32) (net.Conn, *types.Instance, error)
	DialInInstance(ctx context.Context, namespace, instanceID string, port uint32) (net.Conn, *types.Instance, error)

	// Lifecycle operations
	GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error)
	RestartService(ctx context.Context, namespace, serviceName string) (templateGeneration int64, scale int, err error)
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

	// SetEndpointPublisher wires the networking data plane (RUNE-063).
	// Optional; nil-safe. LATE-BOUND: runed calls this on a live
	// orchestrator after agent identity exists — safe while reconciles
	// run (the instance controller swaps the pair atomically).
	SetEndpointPublisher(publisher wiring.EndpointPublisher, nodeID string)

	// SetMountResolver wires the agent-side volumes Subsystem (RUNE-069)
	// into the instance controller. Optional; nil-safe. LATE-BOUND: a
	// not-ready stub is wired at construction and runed swaps in the
	// real subsystem on a live orchestrator after start.
	SetMountResolver(resolver wiring.MountResolver)
}

// instanceOps is the slice of the instance controller the orchestrator
// facade delegates to (RUNE-311 Phase 3). The consumer owns the
// interface; *instancectl.Controller satisfies it.
type instanceOps interface {
	GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error)
	ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error)
	ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error)
	GetInstanceLogs(ctx context.Context, instance *types.Instance, opts types.LogOptions) (io.ReadCloser, error)
	Exec(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error)
	ExecDebug(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error)
	Dial(ctx context.Context, instance *types.Instance, port uint32) (net.Conn, error)
	RestartInstance(ctx context.Context, instance *types.Instance, reason instancectl.RestartReason) error
	StopInstance(ctx context.Context, instance *types.Instance) error
	SetEndpointPublisher(publisher wiring.EndpointPublisher, nodeID string)
	SetMountResolver(resolver wiring.MountResolver)
}

// orchestrator implements the Orchestrator interface
type orchestrator struct {
	// Core dependencies
	store  store.Store
	logger log.Logger

	// Controllers
	serviceController  service.Controller
	instanceController instanceOps
	healthController   health.Controller
	scalingController  scaling.Controller
	volumeController   volume.Controller
	snapshotController volume.SnapshotController

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

	// StorageDriverConfigs is the per-driver configuration block parsed from
	// the runefile [storage] table. Key is the driver name as registered in
	// pkg/storage/driver (e.g. "local", "local-host"); value is the opaque
	// configuration map handed to the driver factory. Optional; nil-safe.
	StorageDriverConfigs map[string]map[string]any

	// DefaultStorageClass mirrors the runefile [storage].defaultStorageClass
	// knob. *string so the empty-string case ("no cluster default") is
	// distinguishable from "unset — keep built-in default".
	DefaultStorageClass *string

	// StoragePreserveOnDelete mirrors the runefile [storage].preserveOnDelete
	// knob. When true, the volume controller demotes ReclaimPolicy:delete
	// to retain for volumes provisioned by the in-tree "local" driver.
	StoragePreserveOnDelete bool

	// StorageSecretLookup resolves `secret:...` references inside
	// StorageClass / Volume parameter maps before the storage drivers
	// see them. Wired by cmd/runed against the store-backed SecretRepo.
	// Nil disables resolution; secret-ref-shaped values then fail the
	// containing operation with a clear error. See RUNE-200 PR 3 and
	// pkg/storage/driverparams.
	StorageSecretLookup driverparams.SecretLookup

	// InitialMountResolver, if set, is installed on the InstanceController
	// before the orchestrator's first reconcile tick. cmd/runed passes a
	// never-ready stub here so the period between orchestrator start and
	// the agent-side volumes Subsystem registering its real resolver is
	// treated as "transient — retry" rather than falling back to using
	// Volume.Handle as the bind source (which would be a UUID for cloud
	// drivers). Without this option set, the controller starts with no
	// resolver and uses the dev/test Handle-fallback path — correct for
	// in-process tests, wrong for production where the agent is racing
	// to come up.
	InitialMountResolver wiring.MountResolver

	// EventLog is the persisted event log (RUNE-126 Phase 2). When set,
	// the instance and volume controllers emit status-transition events
	// for `rune describe`. Nil disables emission; controllers and tests
	// keep working unchanged.
	EventLog events.EventLog
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

	// Create controllers first (needed for finalizer system). The event
	// log is a construction-time dependency (RUNE-311 Phase 3); the
	// endpoint publisher and mount resolver stay late-bound setters.
	var icOpts []instancectl.Option
	if options.EventLog != nil {
		icOpts = append(icOpts, instancectl.WithEventLog(options.EventLog))
	}
	instanceController := instancectl.NewController(
		options.Store,
		options.RunnerManager,
		options.Logger,
		icOpts...,
	)
	if options.InitialMountResolver != nil {
		instanceController.SetMountResolver(options.InitialMountResolver)
	}

	healthController := health.NewController(
		options.Logger,
		options.Store,
		options.RunnerManager,
		instanceController,
	)

	scalingController := scaling.NewController(
		options.Store,
		options.Logger,
	)

	serviceController, err := service.NewController(
		options.Store,
		instanceController,
		healthController,
		options.Logger,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create service controller: %w", err)
	}

	// VolumeController owns the Volume CRUD reconciliation loop
	// (provision/reclaim via the storage driver registry).
	volumeController, err := volume.NewController(volume.Options{
		Store:               options.Store,
		Logger:              options.Logger,
		DriverConfigs:       options.StorageDriverConfigs,
		SecretLookup:        options.StorageSecretLookup,
		DefaultStorageClass: options.DefaultStorageClass,
		PreserveOnDelete:    options.StoragePreserveOnDelete,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create volume controller: %w", err)
	}

	// Wire the persisted event log (RUNE-126 Phase 2) into the
	// controllers that emit status-transition events. Nil-safe.
	if options.EventLog != nil {
		volumeController.SetEventLog(options.EventLog)
		// The reconciler records the update lifecycle (RUNE-042). Without
		// this there is no after-the-fact record of what a rolling update
		// did: Service.Update is cleared the moment it converges.
		serviceController.SetEventLog(options.EventLog)
	}

	// SnapshotController owns the Snapshot CRUD reconciliation loop.
	snapshotController, err := volume.NewSnapshotController(volume.SnapshotOptions{
		Store:         options.Store,
		Logger:        options.Logger,
		DriverConfigs: options.StorageDriverConfigs,
		SecretLookup:  options.StorageSecretLookup,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot controller: %w", err)
	}

	return &orchestrator{
		store:              options.Store,
		logger:             options.Logger.WithComponent("orchestrator"),
		serviceController:  serviceController,
		instanceController: instanceController,
		healthController:   healthController,
		scalingController:  scalingController,
		volumeController:   volumeController,
		snapshotController: snapshotController,
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

	// Health controller must be up before the service controller's first
	// reconcile tick — otherwise AddInstance spawns a monitor goroutine
	// while c.ctx is still nil, the goroutine exits immediately, and
	// later AddInstance calls see "already monitored" and never restart
	// it, leaving instances wedged in Starting forever.
	if err := o.healthController.Start(o.ctx); err != nil {
		return fmt.Errorf("failed to start health controller: %w", err)
	}

	if err := o.serviceController.Start(o.ctx); err != nil {
		return fmt.Errorf("failed to start service controller: %w", err)
	}

	// Start scaling controller
	if err := o.scalingController.Start(o.ctx); err != nil {
		return fmt.Errorf("failed to start scaling controller: %w", err)
	}

	// Start volume controller
	if err := o.volumeController.Start(o.ctx); err != nil {
		return fmt.Errorf("failed to start volume controller: %w", err)
	}

	// Start snapshot controller
	if err := o.snapshotController.Start(o.ctx); err != nil {
		return fmt.Errorf("failed to start snapshot controller: %w", err)
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

	// Stop volume controller
	if err := o.volumeController.Stop(); err != nil {
		o.logger.Error("Failed to stop volume controller", log.Err(err))
	}

	// Stop snapshot controller
	if err := o.snapshotController.Stop(); err != nil {
		o.logger.Error("Failed to stop snapshot controller", log.Err(err))
	}

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

	// Seed metadata defaults at the single choke point every create path
	// funnels through (API CreateService pre-fills these; cast/releasectl does
	// not — its services used to land with Generation 0, no
	// TemplateGeneration, and no LastNonZeroScale, which broke the
	// restart-a-stopped-service restore). Guarded assignments so callers that
	// set explicit values win.
	if service.Metadata == nil {
		service.Metadata = &types.ServiceMetadata{}
	}
	now := time.Now()
	if service.Metadata.CreatedAt.IsZero() {
		service.Metadata.CreatedAt = now
	}
	service.Metadata.UpdatedAt = now
	if service.Metadata.Generation == 0 {
		service.Metadata.Generation = 1
	}
	if service.Metadata.TemplateGeneration == 0 {
		service.Metadata.TemplateGeneration = service.Metadata.Generation
	}
	if service.Metadata.LastNonZeroScale == 0 && service.Scale > 0 {
		service.Metadata.LastNonZeroScale = service.Scale
	}

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

func (o *orchestrator) DebugInInstance(ctx context.Context, namespace, instanceID string, options types.ExecOptions) (types.ExecStream, error) {
	instance, err := o.store.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}
	return o.instanceController.ExecDebug(ctx, instance, options)
}

// Port-forward operations (RUNE-122)

func (o *orchestrator) DialInService(ctx context.Context, namespace, serviceName string, port uint32) (net.Conn, *types.Instance, error) {
	return o.serviceController.DialInService(ctx, namespace, serviceName, port)
}

func (o *orchestrator) DialInInstance(ctx context.Context, namespace, instanceID string, port uint32) (net.Conn, *types.Instance, error) {
	instance, err := o.store.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get instance: %w", err)
	}
	conn, err := o.instanceController.Dial(ctx, instance, port)
	if err != nil {
		return nil, nil, err
	}
	return conn, instance, nil
}

// Lifecycle operations

func (o *orchestrator) GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	return o.instanceController.GetInstanceByID(ctx, namespace, instanceID)
}

func (o *orchestrator) RestartService(ctx context.Context, namespace, serviceName string) (int64, int, error) {
	return o.serviceController.RestartService(ctx, namespace, serviceName)
}

func (o *orchestrator) RestartInstance(ctx context.Context, namespace, instanceID string) error {
	// Get the instance from store
	instance, err := o.store.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}

	// Delegate to instance controller for restart
	return o.instanceController.RestartInstance(ctx, instance, instancectl.RestartReasonManual)
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

// SetEndpointPublisher wires the networking data plane (RUNE-063)
// to the underlying instance controller.
func (o *orchestrator) SetEndpointPublisher(publisher wiring.EndpointPublisher, nodeID string) {
	o.instanceController.SetEndpointPublisher(publisher, nodeID)
}

// SetMountResolver wires the agent-side volumes Subsystem (RUNE-069)
// to the underlying instance controller.
func (o *orchestrator) SetMountResolver(resolver wiring.MountResolver) {
	o.instanceController.SetMountResolver(resolver)
}

// Compile-time proof the real controller satisfies the facade's slice.
var _ instanceOps = (*instancectl.Controller)(nil)
