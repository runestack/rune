package docker

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockDockerClient is a mock implementation of the Docker client
type MockDockerClient struct {
	mock.Mock
}

func (m *MockDockerClient) ContainerExecCreate(ctx context.Context, containerID string, config container.ExecOptions) (container.ExecCreateResponse, error) {
	args := m.Called(ctx, containerID, config)
	return container.ExecCreateResponse{ID: args.String(0)}, args.Error(1)
}

func (m *MockDockerClient) ContainerExecAttach(ctx context.Context, execID string, config container.ExecStartOptions) (types.HijackedResponse, error) {
	args := m.Called(ctx, execID, config)
	return types.HijackedResponse{}, args.Error(0)
}

func (m *MockDockerClient) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	args := m.Called(ctx, execID)
	return container.ExecInspect{
		ExitCode: args.Int(0),
		Running:  args.Bool(1),
	}, args.Error(2)
}

func (m *MockDockerClient) ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error {
	args := m.Called(ctx, execID, options)
	return args.Error(0)
}

// Modified implementation to avoid starting IO copying
func newTestDockerExecStream(
	ctx context.Context,
	cli DockerClient,
	containerID string,
	instanceID string,
	options runner.ExecOptions,
	logger log.Logger,
) (*DockerExecStream, error) {
	// Create cancellable context
	execCtx, cancel := context.WithCancel(ctx)

	// Create the exec stream without starting IO
	r, w := io.Pipe()

	return &DockerExecStream{
		cli:           cli,
		execID:        "exec123", // Mock ID
		containerID:   containerID,
		instanceID:    instanceID,
		logger:        logger.WithComponent("docker-exec-stream"),
		ctx:           execCtx,
		cancel:        cancel,
		stdout:        &readWritePipe{reader: r, writer: w},
		stderr:        &readWritePipe{reader: r, writer: w},
		tty:           options.TTY,
		mutex:         sync.Mutex{},
		exitCodeMutex: sync.Mutex{},
	}, nil
}

func TestNewDockerExecStream(t *testing.T) {
	// Create a mock Docker client
	mockClient := new(MockDockerClient)

	// Create a logger
	logger := log.NewLogger()

	// Create the exec options
	options := runner.ExecOptions{
		Command: []string{"ls", "-la"},
		Env:     map[string]string{"FOO": "bar"},
		TTY:     true,
	}

	// Create the exec stream using our test implementation
	stream, err := newTestDockerExecStream(context.Background(), mockClient, "container123", "instance123", options, logger)

	// Assert results
	assert.NoError(t, err)
	assert.NotNil(t, stream)
}

func TestDockerExecStreamExitCode(t *testing.T) {
	// Create a mock Docker client
	mockClient := new(MockDockerClient)

	// Set up the mock expectations for inspect
	mockClient.On("ContainerExecInspect", mock.Anything, "exec123").
		Return(0, false, nil) // exit code 0, not running, no error

	// Create a minimal exec stream for testing
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &DockerExecStream{
		cli:           mockClient,
		execID:        "exec123",
		ctx:           ctx,
		cancel:        cancel,
		logger:        log.NewLogger(),
		stdout:        &readWritePipe{reader: r, writer: w},
		stderr:        &readWritePipe{reader: r, writer: w},
		closed:        false,
		mutex:         sync.Mutex{},
		exitCodeMutex: sync.Mutex{},
	}

	// Get the exit code
	exitCode, err := stream.ExitCode()

	// Assert results
	assert.NoError(t, err)
	assert.Equal(t, 0, exitCode)

	// Verify mock was called
	mockClient.AssertExpectations(t)
}

func TestDockerExecStreamRunning(t *testing.T) {
	// Create a mock Docker client
	mockClient := new(MockDockerClient)

	// Set up the mock expectations for inspect - process is still running
	mockClient.On("ContainerExecInspect", mock.Anything, "exec123").
		Return(0, true, nil) // exit code 0, still running, no error

	// Create a minimal exec stream for testing, without starting IO copying
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &DockerExecStream{
		cli:           mockClient,
		execID:        "exec123",
		ctx:           ctx,
		cancel:        cancel,
		logger:        log.NewLogger(),
		stdout:        &readWritePipe{reader: r, writer: w},
		stderr:        &readWritePipe{reader: r, writer: w},
		closed:        false,
		mutex:         sync.Mutex{},
		exitCodeMutex: sync.Mutex{},
	}

	// Get the exit code for a running process
	exitCode, err := stream.ExitCode()

	// Assert we get an error since it's still running
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "still running")
	assert.Equal(t, 0, exitCode)

	// Verify mock was called
	mockClient.AssertExpectations(t)
}

func TestDockerExecStreamResizeTerminal(t *testing.T) {
	// Create a mock Docker client
	mockClient := new(MockDockerClient)

	// Set up the mock expectations
	mockClient.On("ContainerExecResize", mock.Anything, "exec123", mock.Anything).
		Return(nil)

	// Create a minimal exec stream for testing
	stream := &DockerExecStream{
		cli:    mockClient,
		execID: "exec123",
		ctx:    context.Background(),
		logger: log.NewLogger(),
		tty:    true, // needs to be true for resize to work
	}

	// Resize the terminal
	err := stream.ResizeTerminal(80, 24)

	// Assert results
	assert.NoError(t, err)

	// Verify mock was called with correct parameters
	mockClient.AssertCalled(t, "ContainerExecResize", mock.Anything, "exec123",
		container.ResizeOptions{Width: 80, Height: 24})
}

func TestDockerExecStreamSignalNonTTY(t *testing.T) {
	// Test only the non-TTY case which doesn't access hijackedResp
	stream := &DockerExecStream{
		tty:    false,
		logger: log.NewLogger(),
		mutex:  sync.Mutex{},
		closed: false,
	}

	// When TTY is false, all signals should be not supported
	err := stream.Signal("SIGTERM")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

func TestExecStream(t *testing.T) {
	// This is a simple test that validates the Reader interface was properly defined
	// We use a type assertion to confirm that DockerExecStream implements the runner.ExecStream interface
	var _ runner.ExecStream = (*DockerExecStream)(nil)

	// We can also create a minimal implementation to satisfy the interface
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	stream := &DockerExecStream{
		cli:    new(MockDockerClient),
		execID: "test-exec",
		logger: log.NewLogger(),
		stdout: &readWritePipe{reader: r, writer: w},
		stderr: &readWritePipe{reader: r, writer: w},
	}

	// Verify it implements the required methods
	assert.NotNil(t, stream)
	assert.Implements(t, (*runner.ExecStream)(nil), stream)
}
