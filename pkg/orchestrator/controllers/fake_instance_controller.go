package controllers

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
)

// FakeInstanceController is the test double for *InstanceController. One
// fake satisfies every consumer's role interface structurally (RUNE-311
// Phase 3) — its stateful core is shared by reconciler and service
// controller tests, so it is deliberately not split per role.
type FakeInstanceController struct {
	// WithdrawServiceInstancesCalls counts batch withdrawals (RUNE-042).
	WithdrawServiceInstancesCalls int

	mu                       sync.Mutex
	logger                   log.Logger
	instances                map[string]*types.Instance // Keyed by ID
	CreateInstanceCalls      []CreateInstanceCall
	RetryCreateInstanceCalls []RetryCreateInstanceCall
	RecreateInstanceCalls    []RecreateInstanceCall
	UpdateInstanceCalls      []UpdateInstanceCall
	StopInstanceCalls        []StopInstanceCall
	DeleteInstanceCalls      []DeleteInstanceCall
	RestartInstanceCalls     []RestartInstanceCall
	GetStatusCalls           []string // Instance IDs
	GetLogsCalls             []string // Instance IDs
	ExecCalls                []ExecCall

	// Port-forward (RUNE-122)
	DialCalls    []FakeDialControllerCall
	DialError    error
	LastDialPeer net.Conn

	// Custom behavior options
	CreateInstanceFunc      func(ctx context.Context, service *types.Service, instanceName string, ordinal int) (*types.Instance, error)
	RetryCreateInstanceFunc func(ctx context.Context, service *types.Service, instance *types.Instance) error
	RecreateInstanceFunc    func(ctx context.Context, service *types.Service, instance *types.Instance) (*types.Instance, error)
	UpdateInstanceFunc      func(ctx context.Context, service *types.Service, instance *types.Instance) error
	StopInstanceFunc        func(ctx context.Context, instance *types.Instance) error
	DeleteInstanceFunc      func(ctx context.Context, instance *types.Instance) error
	RestartInstanceFunc     func(ctx context.Context, instance *types.Instance, reason InstanceRestartReason) error
	GetStatusFunc           func(ctx context.Context, instance *types.Instance) (*types.InstanceStatusInfo, error)
	GetLogsFunc             func(ctx context.Context, instance *types.Instance, opts types.LogOptions) (io.ReadCloser, error)
	ExecFunc                func(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error)
	IsCompatibleFunc        func(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string)

	// Default error responses
	CreateInstanceError      error
	RetryCreateInstanceError error
	RecreateInstanceError    error
	UpdateInstanceError      error
	StopInstanceError        error
	DeleteInstanceError      error
	RestartInstanceError     error
	GetStatusError           error
	GetLogsError             error
	ExecError                error

	// Mock responses
	ExecStdout   []byte
	ExecStderr   []byte
	ExecExitCode int
}

// CreateInstanceCall records the parameters of a CreateInstance call
type CreateInstanceCall struct {
	Service      *types.Service
	InstanceName string
	Ordinal      int
}

// RecreateInstanceCall records the parameters of a RecreateInstance call
type RecreateInstanceCall struct {
	Service  *types.Service
	Instance *types.Instance
}

// RetryCreateInstanceCall records the parameters of a RetryCreateInstance call
type RetryCreateInstanceCall struct {
	Service  *types.Service
	Instance *types.Instance
}

// UpdateInstanceCall records the parameters of an UpdateInstance call
type UpdateInstanceCall struct {
	Service  *types.Service
	Instance *types.Instance
}

// DeleteInstanceCall records the parameters of a DeleteInstance call
type DeleteInstanceCall struct {
	Instance *types.Instance
}

// RestartInstanceCall records the parameters of a RestartInstance call
type RestartInstanceCall struct {
	Instance *types.Instance
	Reason   InstanceRestartReason
}

// ExecCall records the parameters of an Exec call
type ExecCall struct {
	Instance *types.Instance
	Options  types.ExecOptions
}

// StopInstanceCall records the parameters of a StopInstance call
type StopInstanceCall struct {
	Instance *types.Instance
}

// NewFakeInstanceController creates a new test instance controller
func NewFakeInstanceController() *FakeInstanceController {
	return &FakeInstanceController{
		instances: make(map[string]*types.Instance),
		logger:    log.NewLogger().WithComponent("test-instance-controller"),
	}
}

// GetInstanceByID implements InstanceController interface
func (c *FakeInstanceController) GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	return nil, nil
}

// ListInstances records a call to list instances
func (c *FakeInstanceController) ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Convert to pointers
	result := make([]*types.Instance, len(c.instances))
	i := 0
	for _, instance := range c.instances {
		result[i] = instance
		i++
	}
	return result, nil
}

func (c *FakeInstanceController) ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	instances, err := c.collectRunningInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list running instances: %w", err)
	}
	runningInstances := make([]*types.Instance, 0, len(instances))
	for _, instance := range instances {
		if instance.Instance.Status == types.InstanceStatusRunning && instance.Instance.Namespace == namespace {
			runningInstances = append(runningInstances, instance.Instance)
		}
	}
	return runningInstances, nil
}

// CreateInstance records a call to create an instance and returns a predefined or mocked result
func (c *FakeInstanceController) CreateInstance(ctx context.Context, service *types.Service, instanceName string, ordinal int) (*types.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.CreateInstanceCalls = append(c.CreateInstanceCalls, CreateInstanceCall{
		Service:      service,
		InstanceName: instanceName,
		Ordinal:      ordinal,
	})

	// If custom function is provided, use it
	if c.CreateInstanceFunc != nil {
		return c.CreateInstanceFunc(ctx, service, instanceName, ordinal)
	}

	// If error is set, return it
	if c.CreateInstanceError != nil {
		return nil, c.CreateInstanceError
	}

	// Default behavior
	instance := &types.Instance{
		ID:          instanceName, // Use the name as ID for simplicity in tests
		Name:        instanceName,
		Ordinal:     ordinal,
		Namespace:   service.Namespace,
		ServiceID:   service.ID,
		ServiceName: service.Name,
		Status:      types.InstanceStatusRunning,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Environment: make(map[string]string),
	}

	// Store the instance
	c.instances[instance.ID] = instance

	return instance, nil
}

// RetryCreateInstance records a call to retry the create pipeline on an
// existing record. Default behavior: no-op success (record stays as-is
// in the fake's instance map). Tests that need to observe retry calls
// inspect RetryCreateInstanceCalls; tests that need to simulate retry
// failure set RetryCreateInstanceError or RetryCreateInstanceFunc.
func (c *FakeInstanceController) RetryCreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.RetryCreateInstanceCalls = append(c.RetryCreateInstanceCalls, RetryCreateInstanceCall{
		Service:  service,
		Instance: instance,
	})

	if c.RetryCreateInstanceFunc != nil {
		return c.RetryCreateInstanceFunc(ctx, service, instance)
	}
	if c.RetryCreateInstanceError != nil {
		return c.RetryCreateInstanceError
	}
	return nil
}

// RecreateInstance records a call to recreate an instance
func (c *FakeInstanceController) RecreateInstance(ctx context.Context, service *types.Service, instance *types.Instance) (*types.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.RecreateInstanceCalls = append(c.RecreateInstanceCalls, RecreateInstanceCall{
		Service:  service,
		Instance: instance,
	})

	// If custom function is provided, use it
	if c.RecreateInstanceFunc != nil {
		return c.RecreateInstanceFunc(ctx, service, instance)
	}

	// If error is set, return it
	if c.RecreateInstanceError != nil {
		return nil, c.RecreateInstanceError
	}

	// Default behavior: Delete and recreate
	delete(c.instances, instance.ID)

	newInstance := &types.Instance{
		ID:          instance.ID,
		Name:        instance.Name,
		Namespace:   service.Namespace,
		ServiceID:   service.ID,
		ServiceName: service.Name,
		Status:      types.InstanceStatusRunning,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Environment: make(map[string]string),
	}

	// Store the new instance
	c.instances[newInstance.ID] = newInstance

	return newInstance, nil
}

// UpdateInstance records a call to update an instance
func (c *FakeInstanceController) UpdateInstance(ctx context.Context, service *types.Service, instance *types.Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.UpdateInstanceCalls = append(c.UpdateInstanceCalls, UpdateInstanceCall{
		Service:  service,
		Instance: instance,
	})

	// If custom function is provided, use it
	if c.UpdateInstanceFunc != nil {
		return c.UpdateInstanceFunc(ctx, service, instance)
	}

	// If error is set, return it
	if c.UpdateInstanceError != nil {
		return c.UpdateInstanceError
	}

	// Default behavior
	if existingInstance, exists := c.instances[instance.ID]; exists {
		// Update the stored instance
		existingInstance.Status = instance.Status
		existingInstance.UpdatedAt = time.Now()
		existingInstance.Environment = instance.Environment
		c.instances[instance.ID] = existingInstance
	}

	return nil
}

// DeleteInstance records a call to delete an instance
func (c *FakeInstanceController) DeleteInstance(ctx context.Context, instance *types.Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.DeleteInstanceCalls = append(c.DeleteInstanceCalls, DeleteInstanceCall{
		Instance: instance,
	})

	// If custom function is provided, use it
	if c.DeleteInstanceFunc != nil {
		return c.DeleteInstanceFunc(ctx, instance)
	}

	// If error is set, return it
	if c.DeleteInstanceError != nil {
		return c.DeleteInstanceError
	}

	// Default behavior
	delete(c.instances, instance.ID)

	return nil
}

// GetInstanceStatus records a call to get instance status
func (c *FakeInstanceController) GetInstanceStatus(ctx context.Context, instance *types.Instance) (*types.InstanceStatusInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.GetStatusCalls = append(c.GetStatusCalls, instance.ID)

	// If custom function is provided, use it
	if c.GetStatusFunc != nil {
		return c.GetStatusFunc(ctx, instance)
	}

	// If error is set, return it
	if c.GetStatusError != nil {
		return nil, c.GetStatusError
	}

	// Default behavior
	storedInstance, exists := c.instances[instance.ID]
	if !exists {
		return &types.InstanceStatusInfo{
			Status:     types.InstanceStatusUnknown,
			InstanceID: instance.ID,
		}, nil
	}

	return &types.InstanceStatusInfo{
		Status:     storedInstance.Status,
		InstanceID: storedInstance.ID,
		NodeID:     storedInstance.NodeID,
		CreatedAt:  storedInstance.CreatedAt,
	}, nil

}

func (c *FakeInstanceController) isInstanceCompatibleWithService(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string) {
	if c.IsCompatibleFunc != nil {
		return c.IsCompatibleFunc(ctx, instance, service)
	}
	return true, ""
}

// SetEndpointPublisher is a no-op on the fake.
func (c *FakeInstanceController) SetEndpointPublisher(_ EndpointPublisher, _ string) {}

// RepublishServiceByInstance is a no-op on the fake. Tests that need
// to observe republish calls should track them via a custom field.
func (c *FakeInstanceController) RepublishServiceByInstance(_ context.Context, _ *types.Instance) {
}

// RepublishService is a no-op on the fake.
func (c *FakeInstanceController) RepublishService(_ context.Context, _ *types.Service) {
}

// SetMountResolver is a no-op on the fake.
func (c *FakeInstanceController) SetMountResolver(_ MountResolver) {}

// SetEventLog is a no-op on the fake.
func (c *FakeInstanceController) SetEventLog(_ events.EventLog) {}

func (c *FakeInstanceController) collectRunningInstances(ctx context.Context) (map[string]*RunningInstance, error) {
	instances := make(map[string]*RunningInstance)
	for id, instance := range c.instances {
		instances[id] = &RunningInstance{
			Instance: instance,
		}
	}
	return instances, nil
}

// GetInstanceLogs records a call to get instance logs
func (c *FakeInstanceController) GetInstanceLogs(ctx context.Context, instance *types.Instance, opts types.LogOptions) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.GetLogsCalls = append(c.GetLogsCalls, instance.ID)

	// If custom function is provided, use it
	if c.GetLogsFunc != nil {
		return c.GetLogsFunc(ctx, instance, opts)
	}

	// If error is set, return it
	if c.GetLogsError != nil {
		return nil, c.GetLogsError
	}

	// Default behavior - return empty reader
	return io.NopCloser(strings.NewReader("")), nil
}

// Dial returns one end of a net.Pipe; the peer end is stashed on the
// fake controller for tests.
func (c *FakeInstanceController) Dial(ctx context.Context, instance *types.Instance, port uint32) (net.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.DialCalls = append(c.DialCalls, FakeDialControllerCall{Instance: instance, Port: port})
	if c.DialError != nil {
		return nil, c.DialError
	}
	local, remote := net.Pipe()
	c.LastDialPeer = remote
	return local, nil
}

// FakeDialControllerCall records one Dial invocation on the fake controller.
type FakeDialControllerCall struct {
	Instance *types.Instance
	Port     uint32
}

// Exec records a call to execute a command in an instance
func (c *FakeInstanceController) Exec(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.ExecCalls = append(c.ExecCalls, ExecCall{
		Instance: instance,
		Options:  options,
	})

	// If custom function is provided, use it
	if c.ExecFunc != nil {
		return c.ExecFunc(ctx, instance, options)
	}

	// If error is set, return it
	if c.ExecError != nil {
		return nil, c.ExecError
	}

	// Default behavior - return a fake exec stream
	return runner.NewFakeExecStream(c.ExecStdout, c.ExecStderr, c.ExecExitCode), nil
}

// ExecDebug stubs the debug-sidecar path for tests. The fake doesn't
// model the sidecar lifecycle (that's covered by the real docker runner
// tests + manual e2e); we just return a fake stream so callers exercising
// the orchestrator surface don't blow up.
func (c *FakeInstanceController) ExecDebug(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ExecError != nil {
		return nil, c.ExecError
	}
	return runner.NewFakeExecStream(c.ExecStdout, c.ExecStderr, c.ExecExitCode), nil
}

// RestartInstance records a call to restart an instance
func (c *FakeInstanceController) RestartInstance(ctx context.Context, instance *types.Instance, reason InstanceRestartReason) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.RestartInstanceCalls = append(c.RestartInstanceCalls, RestartInstanceCall{
		Instance: instance,
		Reason:   reason,
	})

	// If custom function is provided, use it
	if c.RestartInstanceFunc != nil {
		return c.RestartInstanceFunc(ctx, instance, reason)
	}

	// If error is set, return it
	if c.RestartInstanceError != nil {
		return c.RestartInstanceError
	}

	// Default behavior
	if storedInstance, exists := c.instances[instance.ID]; exists {
		storedInstance.Status = types.InstanceStatusRunning
		storedInstance.UpdatedAt = time.Now()
		c.instances[instance.ID] = storedInstance
	}

	return nil
}

// WithdrawServiceInstances records a batch withdrawal (RUNE-042 Phase 0).
// The fake mirrors the real controller's contract: Running instances are
// flipped to Terminating in place; no drain is taken.
func (c *FakeInstanceController) WithdrawServiceInstances(ctx context.Context, service *types.Service, instances []*types.Instance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WithdrawServiceInstancesCalls++
	for _, inst := range instances {
		if inst != nil && inst.Status == types.InstanceStatusRunning {
			inst.Status = types.InstanceStatusTerminating
		}
	}
}

// classifyInstance mirrors the real controller's classifier for the fake:
// it defers to whatever isInstanceCompatibleWithService is configured to
// return, mapping incompatible to CompatBroken (the pre-RUNE-042 meaning of
// `false`, and the class that still triggers immediate replacement).
func (c *FakeInstanceController) classifyInstance(ctx context.Context, instance *types.Instance, service *types.Service) CompatVerdict {
	ok, reason := c.isInstanceCompatibleWithService(ctx, instance, service)
	if ok {
		return CompatVerdict{Class: CompatOK}
	}
	return CompatVerdict{Class: CompatBroken, Reason: reason}
}

// StopInstance records a call to stop an instance
func (c *FakeInstanceController) StopInstance(ctx context.Context, instance *types.Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Record the call
	c.StopInstanceCalls = append(c.StopInstanceCalls, StopInstanceCall{
		Instance: instance,
	})

	// If custom function is provided, use it
	if c.StopInstanceFunc != nil {
		return c.StopInstanceFunc(ctx, instance)
	}

	// If error is set, return it
	if c.StopInstanceError != nil {
		return c.StopInstanceError
	}

	// Default behavior
	if storedInstance, exists := c.instances[instance.ID]; exists {
		storedInstance.Status = types.InstanceStatusStopped
		storedInstance.UpdatedAt = time.Now()
		storedInstance.StatusMessage = "Stopped by user"
		c.instances[instance.ID] = storedInstance
	}

	return nil
}

// AddInstance allows tests to add an instance directly to the controller's storage
func (c *FakeInstanceController) AddInstance(instance *types.Instance) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.instances[instance.ID] = instance
}

// GetInstance allows tests to retrieve an instance from the controller's storage
func (c *FakeInstanceController) GetInstance(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	instance, exists := c.instances[instanceID]
	if !exists {
		return nil, fmt.Errorf("instance not found: %s", instanceID)
	}

	return instance, nil
}

// Reset clears all recorded calls and stored instances
func (c *FakeInstanceController) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.instances = make(map[string]*types.Instance)
	c.CreateInstanceCalls = nil
	c.RecreateInstanceCalls = nil
	c.UpdateInstanceCalls = nil
	c.StopInstanceCalls = nil
	c.DeleteInstanceCalls = nil
	c.RestartInstanceCalls = nil
	c.GetStatusCalls = nil
	c.GetLogsCalls = nil
	c.ExecCalls = nil
}

// Compile-time proof the fake satisfies every consumer's role interface.
var (
	_ reconcilerInstanceOps = (*FakeInstanceController)(nil)
	_ serviceInstanceOps    = (*FakeInstanceController)(nil)
	_ healthInstanceOps     = (*FakeInstanceController)(nil)
)
