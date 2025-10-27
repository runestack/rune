package orchestrator

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// Compile-time check that FakeOrchestrator implements Orchestrator interface
var _ Orchestrator = (*FakeOrchestrator)(nil)

// FakeOrchestrator provides a simple fake implementation for testing
// It implements the Orchestrator interface
type FakeOrchestrator struct {
	mu sync.RWMutex

	// Fake data
	services  map[string]*types.Service
	instances map[string]*types.Instance

	// Exec-related fields for testing
	ExecStdout   []byte
	ExecStderr   []byte
	ExecExitCode int

	// Track calls for testing
	ExecInInstanceCalls []ExecInInstanceCall

	// Lifecycle
	started bool
	stopped bool
}

// ExecInInstanceCall tracks ExecInInstance calls
type ExecInInstanceCall struct {
	Namespace  string
	ServiceID  string
	InstanceID string
	Options    types.ExecOptions
	Stream     types.ExecStream
	Error      error
}

// NewFakeOrchestrator creates a new fake orchestrator for testing
func NewFakeOrchestrator() *FakeOrchestrator {
	return &FakeOrchestrator{
		services:  make(map[string]*types.Service),
		instances: make(map[string]*types.Instance),
	}
}

// AddService adds a service to the fake orchestrator
func (fo *FakeOrchestrator) AddService(service *types.Service) {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	key := service.Namespace + "/" + service.Name
	fo.services[key] = service
}

// AddInstance adds an instance to the fake orchestrator
func (fo *FakeOrchestrator) AddInstance(instance *types.Instance) {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	fo.instances[instance.ID] = instance
}

// GetInstanceByID implements Orchestrator interface
func (fo *FakeOrchestrator) GetInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	fo.mu.RLock()
	defer fo.mu.RUnlock()
	inst, ok := fo.instances[instanceID]
	if !ok {
		return nil, errors.New("resource not found")
	}
	return inst, nil
}

// ListDeletionOperations implements Orchestrator interface
func (fo *FakeOrchestrator) ListDeletionOperations(ctx context.Context, namespace string) ([]*types.DeletionOperation, error) {
	return nil, nil
}

// ListInstances implements Orchestrator interface
func (fo *FakeOrchestrator) ListInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	fo.mu.RLock()
	defer fo.mu.RUnlock()
	var out []*types.Instance
	for _, inst := range fo.instances {
		if namespace == "" || namespace == "*" || inst.Namespace == namespace {
			out = append(out, inst)
		}
	}
	return out, nil
}

// ListRunningInstances implements Orchestrator interface
func (fo *FakeOrchestrator) ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error) {
	fo.mu.RLock()
	defer fo.mu.RUnlock()
	var out []*types.Instance
	for _, inst := range fo.instances {
		if (namespace == "" || namespace == "*" || inst.Namespace == namespace) && inst.Status == types.InstanceStatusRunning {
			out = append(out, inst)
		}
	}
	return out, nil
}

// WatchServices implements Orchestrator interface
func (fo *FakeOrchestrator) WatchServices(ctx context.Context, namespace string) (<-chan store.WatchEvent, error) {
	return nil, nil
}

// Start implements Orchestrator interface
func (fo *FakeOrchestrator) Start(ctx context.Context) error {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	fo.started = true
	return nil
}

// Stop implements Orchestrator interface
func (fo *FakeOrchestrator) Stop() error {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	fo.stopped = true
	return nil
}

// CreateService implements Orchestrator interface
func (fo *FakeOrchestrator) CreateService(ctx context.Context, service *types.Service) error {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	key := service.Namespace + "/" + service.Name
	fo.services[key] = service
	return nil
}

// ScaleService updates the scale of a service in the fake orchestrator
func (fo *FakeOrchestrator) ScaleService(ctx context.Context, namespace, name string, scale int) error {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	key := namespace + "/" + name
	svc, ok := fo.services[key]
	if !ok {
		return errors.New("resource not found")
	}
	svc.Scale = scale
	return nil
}

// UpdateService implements Orchestrator interface
func (fo *FakeOrchestrator) UpdateService(ctx context.Context, service *types.Service) error {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	key := service.Namespace + "/" + service.Name
	fo.services[key] = service
	return nil
}

// DeleteService implements Orchestrator interface
func (fo *FakeOrchestrator) DeleteService(ctx context.Context, request *types.DeletionRequest) (*types.DeletionResponse, error) {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	key := request.Namespace + "/" + request.Name
	delete(fo.services, key)
	return &types.DeletionResponse{
		DeletionID: "fake-deletion-" + request.Name,
		Status:     "pending",
	}, nil
}

// GetService implements Orchestrator interface
func (fo *FakeOrchestrator) GetService(ctx context.Context, namespace, name string) (*types.Service, error) {
	fo.mu.RLock()
	defer fo.mu.RUnlock()
	key := namespace + "/" + name
	service, exists := fo.services[key]
	if !exists {
		return nil, errors.New("resource not found")
	}
	return service, nil
}

// ListServices implements Orchestrator interface
func (fo *FakeOrchestrator) ListServices(ctx context.Context, namespace string) ([]*types.Service, error) {
	fo.mu.RLock()
	defer fo.mu.RUnlock()
	var services []*types.Service
	for _, service := range fo.services {
		// Support all-namespaces queries when namespace is empty or "*"
		if namespace == "" || namespace == "*" || service.Namespace == namespace {
			services = append(services, service)
		}
	}
	return services, nil
}

// GetInstanceStatus implements Orchestrator interface
func (fo *FakeOrchestrator) GetInstanceStatus(ctx context.Context, namespace, instanceID string) (*types.InstanceStatusInfo, error) {
	fo.mu.RLock()
	defer fo.mu.RUnlock()
	instance, exists := fo.instances[instanceID]
	if !exists {
		return nil, errors.New("resource not found")
	}
	return &types.InstanceStatusInfo{
		Status:        instance.Status,
		StatusMessage: "Instance status from fake orchestrator",
		InstanceID:    instance.ID,
		NodeID:        instance.NodeID,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}, nil
}

// GetServiceLogs implements Orchestrator interface
func (fo *FakeOrchestrator) GetServiceLogs(ctx context.Context, namespace, name string, opts types.LogOptions) (io.ReadCloser, error) {
	return &fakeLogReader{data: []byte("fake service logs")}, nil
}

// GetInstanceLogs implements Orchestrator interface
func (fo *FakeOrchestrator) GetInstanceLogs(ctx context.Context, namespace, instanceID string, options types.LogOptions) (io.ReadCloser, error) {
	return &fakeLogReader{data: []byte("fake logs")}, nil
}

// fakeLogReader implements io.ReadCloser for logs
type fakeLogReader struct {
	data   []byte
	pos    int
	closed bool
}

func (flr *fakeLogReader) Read(p []byte) (n int, err error) {
	if flr.closed {
		return 0, io.EOF
	}
	if flr.pos >= len(flr.data) {
		return 0, io.EOF
	}
	n = copy(p, flr.data[flr.pos:])
	flr.pos += n
	return n, nil
}

func (flr *fakeLogReader) Close() error {
	flr.closed = true
	return nil
}

// ExecInInstance implements Orchestrator interface
func (fo *FakeOrchestrator) ExecInInstance(ctx context.Context, namespace, instanceID string, options types.ExecOptions) (types.ExecStream, error) {
	fo.mu.RLock()
	defer fo.mu.RUnlock()

	// Create a fake exec stream
	stream := &fakeExecStream{
		stdout:   fo.ExecStdout,
		stderr:   fo.ExecStderr,
		exitCode: fo.ExecExitCode,
	}

	call := ExecInInstanceCall{
		Namespace:  namespace,
		InstanceID: instanceID,
		Options:    options,
		Stream:     stream,
		Error:      nil,
	}
	fo.ExecInInstanceCalls = append(fo.ExecInInstanceCalls, call)

	return stream, nil
}

// ExecInService implements the old orchestrator interface for compatibility
func (fo *FakeOrchestrator) ExecInService(ctx context.Context, namespace, serviceName string, options types.ExecOptions) (types.ExecStream, error) {
	// For fake implementation, just return the same stream as ExecInInstance
	stream := &fakeExecStream{
		stdout:   fo.ExecStdout,
		stderr:   fo.ExecStderr,
		exitCode: fo.ExecExitCode,
	}
	return stream, nil
}

// GetDeletionStatus implements the old orchestrator interface for compatibility
func (fo *FakeOrchestrator) GetDeletionStatus(ctx context.Context, namespace, deletionID string) (*types.DeletionOperation, error) {
	return &types.DeletionOperation{
		ID:          deletionID,
		Namespace:   namespace,
		ServiceName: "test-service",
		Status:      "completed",
		StartTime:   time.Now(),
	}, nil
}

// GetServiceStatus implements the old orchestrator interface for compatibility
func (fo *FakeOrchestrator) GetServiceStatus(ctx context.Context, namespace, serviceName string) (*types.ServiceStatusInfo, error) {
	return &types.ServiceStatusInfo{
		Status: types.ServiceStatusRunning,
	}, nil
}

// RestartService implements the old orchestrator interface for compatibility
func (fo *FakeOrchestrator) RestartService(ctx context.Context, namespace, serviceName string) error {
	return nil
}

// StopService implements the old orchestrator interface for compatibility
func (fo *FakeOrchestrator) StopService(ctx context.Context, namespace, serviceName string) error {
	return nil
}

// RestartInstance implements Orchestrator interface
func (fo *FakeOrchestrator) RestartInstance(ctx context.Context, namespace, instanceID string) error {
	return nil
}

// StopInstance implements Orchestrator interface
func (fo *FakeOrchestrator) StopInstance(ctx context.Context, namespace, instanceID string) error {
	return nil
}

// CreateScalingOperation implements Orchestrator interface
func (fo *FakeOrchestrator) CreateScalingOperation(ctx context.Context, service *types.Service, params types.ScalingOperationParams) error {
	// For fake implementation, immediately apply the target scale
	fo.mu.Lock()
	defer fo.mu.Unlock()
	key := service.Namespace + "/" + service.Name
	if svc, ok := fo.services[key]; ok {
		svc.Scale = params.TargetScale
		// Update pointer passed in as well to reflect change in response
		service.Scale = params.TargetScale
	}
	return nil
}

// GetActiveScalingOperation implements Orchestrator interface
func (fo *FakeOrchestrator) GetActiveScalingOperation(ctx context.Context, namespace, serviceName string) (*types.ScalingOperation, error) {
	// For fake implementation, return nil (no active operation)
	return nil, nil
}

// fakeExecStream implements types.ExecStream for testing
type fakeExecStream struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	closed   bool
}

func (fes *fakeExecStream) Read(p []byte) (n int, err error) {
	if fes.closed || len(fes.stdout) == 0 {
		return 0, io.EOF
	}
	n = copy(p, fes.stdout)
	fes.stdout = fes.stdout[n:]
	return n, nil
}

func (fes *fakeExecStream) Write(p []byte) (n int, err error) {
	if fes.closed {
		return 0, errors.New("stream closed")
	}
	return len(p), nil
}

func (fes *fakeExecStream) Close() error {
	fes.closed = true
	return nil
}

func (fes *fakeExecStream) ExitCode() (int, error) {
	return fes.exitCode, nil
}

func (fes *fakeExecStream) ResizeTerminal(width, height uint32) error {
	return nil
}

func (fes *fakeExecStream) Signal(signal string) error {
	return nil
}

func (fes *fakeExecStream) Stderr() io.Reader {
	return &fakeStderrReader{data: fes.stderr}
}

// fakeStderrReader implements io.Reader for stderr
type fakeStderrReader struct {
	data []byte
	pos  int
}

func (fsr *fakeStderrReader) Read(p []byte) (n int, err error) {
	if fsr.pos >= len(fsr.data) {
		return 0, io.EOF
	}
	n = copy(p, fsr.data[fsr.pos:])
	fsr.pos += n
	return n, nil
}
