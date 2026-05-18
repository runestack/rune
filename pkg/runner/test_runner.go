package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// TestRunner is a simplified, predictable implementation of Runner for testing.
// Instead of requiring expectations to be set up, it returns predefined responses.
type TestRunner struct {
	// Configurable test behavior
	StatusResults map[string]types.InstanceStatus
	Instances     map[string]*types.Instance
	ExecOutput    []byte
	ExecErrOutput []byte
	ExitCodeVal   int
	LogOutput     []byte
	ErrorToReturn error

	// Optional tracking for verification
	CreatedInstances []*types.Instance
	StartedInstances []string
	StoppedInstances []string
	RemovedInstances []string
	ExecCalls        []string
	ExecOptions      []ExecOptions
	LogCalls         []string
	StatusCalls      []string

	// Port-forward tracking (RUNE-122)
	DialCalls    []DialCall
	LastDialPeer net.Conn

	// Init step tracking (RUNE-121)
	InitCalls    []InitCall
	InitExitCode int
	// InitFunc, if non-nil, overrides the default behaviour. It is
	// invoked under r.mu and receives the current 1-based attempt count
	// for this step (so tests can simulate "fail twice then succeed"
	// scenarios). Returning a nil error and exit==0 means success.
	InitFunc func(call int, step types.InitStep) (int, error)
	mu       sync.RWMutex // protects the tracking fields
}

// DialCall captures one Runner.Dial invocation for assertions in tests.
type DialCall struct {
	InstanceID string
	Port       uint32
}

// InitCall captures one Runner.RunInit invocation for assertions in tests.
type InitCall struct {
	InstanceID string
	Step       types.InitStep
}

// GetStartedInstances returns a copy of StartedInstances (thread-safe)
func (r *TestRunner) GetStartedInstances() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]string, len(r.StartedInstances))
	copy(result, r.StartedInstances)
	return result
}

// GetStoppedInstances returns a copy of StoppedInstances (thread-safe)
func (r *TestRunner) GetStoppedInstances() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]string, len(r.StoppedInstances))
	copy(result, r.StoppedInstances)
	return result
}

// GetExecCalls returns a copy of ExecCalls (thread-safe)
func (r *TestRunner) GetExecCalls() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]string, len(r.ExecCalls))
	copy(result, r.ExecCalls)
	return result
}

// NewTestRunner creates a new TestRunner with default behavior
func NewTestRunner() *TestRunner {
	return &TestRunner{
		StatusResults:    make(map[string]types.InstanceStatus),
		Instances:        make(map[string]*types.Instance),
		ExitCodeVal:      0,
		ExecOutput:       []byte("test stdout"),
		ExecErrOutput:    []byte("test stderr"),
		CreatedInstances: make([]*types.Instance, 0),
		StartedInstances: make([]string, 0),
		StoppedInstances: make([]string, 0),
		RemovedInstances: make([]string, 0),
		ExecCalls:        make([]string, 0),
		ExecOptions:      make([]ExecOptions, 0),
		LogCalls:         make([]string, 0),
		StatusCalls:      make([]string, 0),
	}
}

func (r *TestRunner) Type() types.RunnerType {
	return types.RunnerTypeTest
}

// Create tracks instance creation
func (r *TestRunner) Create(ctx context.Context, instance *types.Instance) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ErrorToReturn != nil {
		return r.ErrorToReturn
	}

	r.CreatedInstances = append(r.CreatedInstances, instance)
	r.Instances[instance.ID] = instance
	return nil
}

// Start tracks instance starting
func (r *TestRunner) Start(ctx context.Context, instance *types.Instance) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ErrorToReturn != nil {
		return r.ErrorToReturn
	}

	r.StartedInstances = append(r.StartedInstances, instance.ID)

	// Update status if we're tracking this instance
	if instance, ok := r.Instances[instance.ID]; ok {
		instance.Status = types.InstanceStatusRunning
	}

	return nil
}

// Stop tracks instance stopping
func (r *TestRunner) Stop(ctx context.Context, instance *types.Instance, timeout time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ErrorToReturn != nil {
		return r.ErrorToReturn
	}

	r.StoppedInstances = append(r.StoppedInstances, instance.ID)

	// Update status if we're tracking this instance
	if instance, ok := r.Instances[instance.ID]; ok {
		instance.Status = types.InstanceStatusStopped
	}

	return nil
}

// Rename is a no-op for the test runner; included so it satisfies the
// Runner interface alongside the docker runner where Rename is used
// during failed-instance retention to free a container name.
func (r *TestRunner) Rename(ctx context.Context, instance *types.Instance, newName string) error {
	return nil
}

// RunDebug is unsupported by the test runner; included so it satisfies
// the Runner interface. Returns ErrDebugNotSupported.
func (r *TestRunner) RunDebug(ctx context.Context, instance *types.Instance, options ExecOptions) (ExecStream, error) {
	return nil, ErrDebugNotSupported
}

// Remove tracks instance removal
func (r *TestRunner) Remove(ctx context.Context, instance *types.Instance, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ErrorToReturn != nil {
		return r.ErrorToReturn
	}

	r.RemovedInstances = append(r.RemovedInstances, instance.ID)
	delete(r.Instances, instance.ID)
	return nil
}

// GetLogs returns predefined log output
func (r *TestRunner) GetLogs(ctx context.Context, instance *types.Instance, options LogOptions) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.LogCalls = append(r.LogCalls, instance.ID)

	if r.ErrorToReturn != nil {
		return nil, r.ErrorToReturn
	}

	return io.NopCloser(bytes.NewReader(r.LogOutput)), nil
}

// Status returns predefined status or Running as default
func (r *TestRunner) Status(ctx context.Context, instance *types.Instance) (types.InstanceStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.StatusCalls = append(r.StatusCalls, instance.ID)

	if r.ErrorToReturn != nil {
		return types.InstanceStatusFailed, r.ErrorToReturn
	}

	if status, ok := r.StatusResults[instance.ID]; ok {
		return status, nil
	}

	if instance, ok := r.Instances[instance.ID]; ok {
		return instance.Status, nil
	}

	// Default status
	return types.InstanceStatusRunning, nil
}

// List returns all registered instances
func (r *TestRunner) List(ctx context.Context, namespace string) ([]*types.Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ErrorToReturn != nil {
		return nil, r.ErrorToReturn
	}

	instances := make([]*types.Instance, 0, len(r.Instances))
	for _, instance := range r.Instances {
		instances = append(instances, instance)
	}

	return instances, nil
}

// Exec returns a predefined TestExecStream
func (r *TestRunner) Exec(ctx context.Context, instance *types.Instance, options ExecOptions) (ExecStream, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ExecCalls = append(r.ExecCalls, instance.ID)
	r.ExecOptions = append(r.ExecOptions, options)

	if r.ErrorToReturn != nil {
		return nil, r.ErrorToReturn
	}

	// Return a fake exec stream with our predefined behavior
	return &TestExecStream{
		StdoutContent: r.ExecOutput,
		StderrContent: r.ExecErrOutput,
		ExitCodeVal:   r.ExitCodeVal,
	}, nil
}

// Dial returns one end of a net.Pipe; the other end is exposed via
// LastDialPeer for tests that want to read/write the "remote side."
// DialCalls captures the call. If ErrorToReturn is set the call fails.
func (r *TestRunner) Dial(ctx context.Context, instance *types.Instance, port uint32) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.DialCalls = append(r.DialCalls, DialCall{InstanceID: instance.ID, Port: port})
	if r.ErrorToReturn != nil {
		return nil, r.ErrorToReturn
	}
	local, remote := net.Pipe()
	r.LastDialPeer = remote
	return local, nil
}

// RunInit records the call and returns the configured init exit code.
// Errors are surfaced via ErrorToReturn for parity with other methods.
// For per-attempt control (e.g. retry tests), set InitFunc.
func (r *TestRunner) RunInit(ctx context.Context, instance *types.Instance, step types.InitStep) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.InitCalls = append(r.InitCalls, InitCall{InstanceID: instance.ID, Step: step})
	if r.InitFunc != nil {
		return r.InitFunc(len(r.InitCalls), step)
	}
	if r.ErrorToReturn != nil {
		return r.InitExitCode, r.ErrorToReturn
	}
	return r.InitExitCode, nil
}

// TestExecStream is a predictable implementation of ExecStream for testing
type TestExecStream struct {
	StdoutContent []byte
	StderrContent []byte
	ExitCodeVal   int
	InputCapture  []byte
	SignalsSent   []string
	Resizes       []struct{ Width, Height uint32 }

	stdoutPos    int
	stderrReader *bytes.Reader
	closed       bool
	mu           sync.Mutex
}

// Write captures input that would be sent to the exec process
func (s *TestExecStream) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, io.ErrClosedPipe
	}

	// Capture the input
	s.InputCapture = append(s.InputCapture, p...)
	return len(p), nil
}

// Read returns predefined output content in chunks
func (s *TestExecStream) Read(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, io.ErrClosedPipe
	}

	// If we've read everything, return EOF
	if s.stdoutPos >= len(s.StdoutContent) {
		return 0, io.EOF
	}

	// Calculate how much to read
	remaining := len(s.StdoutContent) - s.stdoutPos
	toRead := len(p)
	if toRead > remaining {
		toRead = remaining
	}

	// Copy the data
	copy(p, s.StdoutContent[s.stdoutPos:s.stdoutPos+toRead])
	s.stdoutPos += toRead

	return toRead, nil
}

// Stderr returns an io.Reader for the stderr stream
func (s *TestExecStream) Stderr() io.Reader {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stderrReader == nil {
		s.stderrReader = bytes.NewReader(s.StderrContent)
	}

	return s.stderrReader
}

// ResizeTerminal records terminal resize events
func (s *TestExecStream) ResizeTerminal(width, height uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return fmt.Errorf("exec session closed")
	}

	s.Resizes = append(s.Resizes, struct{ Width, Height uint32 }{width, height})
	return nil
}

// Signal records signals sent to the process
func (s *TestExecStream) Signal(sigName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return fmt.Errorf("exec session closed")
	}

	s.SignalsSent = append(s.SignalsSent, sigName)
	return nil
}

// ExitCode returns the predefined exit code
func (s *TestExecStream) ExitCode() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If we haven't read all output yet, return an error
	if !s.closed && s.stdoutPos < len(s.StdoutContent) {
		return 0, fmt.Errorf("process still running")
	}

	return s.ExitCodeVal, nil
}

// Close marks the stream as closed
func (s *TestExecStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	return nil
}
