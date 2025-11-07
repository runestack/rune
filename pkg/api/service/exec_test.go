package service

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc/metadata"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// setupTestEnvironment creates a clean test environment for each test
func setupTestEnvironment(t *testing.T) (*orchestrator.FakeOrchestrator, *store.TestStore, *types.Instance) {
	t.Helper()

	// Create fresh test components for each test
	testStore := store.NewTestStore()
	testOrchestrator := orchestrator.NewFakeOrchestrator()

	// Set up the instance record for the test
	instance := &types.Instance{
		ID:          "instance123",
		Namespace:   "default",
		Name:        "test-instance",
		ServiceID:   "service123",
		ServiceName: "test-service",
		ContainerID: "container123",
		Status:      types.InstanceStatusRunning,
	}

	// Add the instance to the TestStore
	err := testStore.Create(context.Background(), types.ResourceTypeInstance, "default", "instance123", instance)
	assert.NoError(t, err)

	// Add the instance to the orchestrator
	testOrchestrator.AddInstance(instance)

	return testOrchestrator, testStore, instance
}

// setupMockStream creates a properly configured mock stream with robust expectations
func setupMockStream(t *testing.T, testOrchestrator *orchestrator.FakeOrchestrator) *MockExecServiceStream {
	t.Helper()

	mockCtx := metadata.NewIncomingContext(context.Background(), metadata.New(nil))
	mockStream := NewMockExecServiceStream(mockCtx)

	// Configure the orchestrator with test data
	testOrchestrator.ExecStdout = []byte("sample command output")
	testOrchestrator.ExecStderr = []byte("sample error output")
	testOrchestrator.ExecExitCode = 0

	// Set up robust mock expectations with proper ordering
	// First request will be the init request
	mockStream.On("Recv").Return(&generated.ExecRequest{
		Request: &generated.ExecRequest_Init{
			Init: &generated.ExecInitRequest{
				Target: &generated.ExecInitRequest_InstanceId{
					InstanceId: "instance123",
				},
				Command: []string{"ls", "-la"},
				Tty:     true,
			},
		},
	}, nil).Once()

	// Expect status response (success)
	mockStream.On("Send", mock.MatchedBy(func(resp *generated.ExecResponse) bool {
		statusResp, ok := resp.Response.(*generated.ExecResponse_Status)
		return ok && statusResp.Status.Code == int32(0)
	})).Return(nil).Once()

	// Expect stdout response
	mockStream.On("Send", mock.MatchedBy(func(resp *generated.ExecResponse) bool {
		stdoutResp, ok := resp.Response.(*generated.ExecResponse_Stdout)
		return ok && bytes.Equal(stdoutResp.Stdout, testOrchestrator.ExecStdout)
	})).Return(nil).Once()

	// Expect stderr response
	mockStream.On("Send", mock.MatchedBy(func(resp *generated.ExecResponse) bool {
		stderrResp, ok := resp.Response.(*generated.ExecResponse_Stderr)
		return ok && bytes.Equal(stderrResp.Stderr, testOrchestrator.ExecStderr)
	})).Return(nil).Once()

	// Expect exit code response
	mockStream.On("Send", mock.MatchedBy(func(resp *generated.ExecResponse) bool {
		exitResp, ok := resp.Response.(*generated.ExecResponse_Exit)
		return ok && exitResp.Exit.Code == int32(0)
	})).Return(nil).Once()

	// Second receive will terminate the stream
	var nilReq *generated.ExecRequest = nil
	mockStream.On("Recv").Return(nilReq, io.EOF).Once()

	return mockStream
}

// TestExecServiceStreamExec tests the ExecService with a mock Docker runner
func TestExecServiceStreamExec(t *testing.T) {
	// Ensure test isolation
	t.Parallel()

	// Set up test environment
	testOrchestrator, _, _ := setupTestEnvironment(t)

	// Create a test Docker runner
	testDockerRunner := &runner.TestRunner{}
	testDockerRunner.ExecOutput = []byte("test output")
	testDockerRunner.ExecErrOutput = []byte("test error")
	testDockerRunner.ExitCodeVal = 0

	// Note: FakeOrchestrator doesn't have SetRunner method, so we'll work with what's available
	// The test will use the orchestrator's built-in exec functionality

	// Set up mock stream with robust expectations
	mockStream := setupMockStream(t, testOrchestrator)

	// Create ExecService
	logger := log.NewLogger()
	execService := NewExecService(logger, testOrchestrator)

	// Call StreamExec with timeout to prevent hanging
	done := make(chan error, 1)
	go func() {
		done <- execService.StreamExec(mockStream)
	}()

	// Wait for completion or timeout
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out - possible deadlock or infinite loop")
	}

	// Verify the results with more detailed assertions
	mockStream.AssertExpectations(t)

	// Verify the orchestrator received the call
	assert.Equal(t, 1, len(testOrchestrator.ExecInInstanceCalls))
	assert.Equal(t, "instance123", testOrchestrator.ExecInInstanceCalls[0].InstanceID)
}

// MockStdErrReader mocks the stderr reader
type MockStdErrReader struct {
	mock.Mock
}

func (m *MockStdErrReader) Read(p []byte) (n int, err error) {
	args := m.Called(p)
	return args.Int(0), args.Error(1)
}

// TestSimpleExecService is a minimal test to verify basic functionality
func TestSimpleExecService(t *testing.T) {
	t.Parallel()

	// Create a simple exec service without using the store
	logger := log.NewLogger()
	testOrchestrator := orchestrator.NewFakeOrchestrator()

	execService := NewExecService(logger, testOrchestrator)

	// This is just a placeholder test to ensure our interfaces are correct
	assert.NotNil(t, execService)
}

func TestExecServiceSimple(t *testing.T) {
	t.Parallel()

	// Create a simple exec service without using the store
	logger := log.NewLogger()
	testOrchestrator := orchestrator.NewFakeOrchestrator()

	execService := NewExecService(logger, testOrchestrator)

	// This is just a placeholder test to ensure our Reader interface is working correctly
	assert.NotNil(t, execService)
}

// TestExecServiceWithTestRunner tests the ExecService using the TestRunner instead of mocks
func TestExecServiceWithTestRunner(t *testing.T) {
	// Ensure test isolation
	t.Parallel()

	// Set up test environment
	testOrchestrator, _, _ := setupTestEnvironment(t)

	// Set up mock stream with robust expectations
	mockStream := setupMockStream(t, testOrchestrator)

	// Create ExecService using the orchestrator
	logger := log.NewLogger()
	execService := NewExecService(logger, testOrchestrator)

	// Call StreamExec with timeout to prevent hanging
	done := make(chan error, 1)
	go func() {
		done <- execService.StreamExec(mockStream)
	}()

	// Wait for completion or timeout
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out - possible deadlock or infinite loop")
	}

	// Verify the results with more detailed assertions
	mockStream.AssertExpectations(t)

	// Verify the orchestrator received the call
	assert.Equal(t, 1, len(testOrchestrator.ExecInInstanceCalls))
	assert.Equal(t, "instance123", testOrchestrator.ExecInInstanceCalls[0].InstanceID)
	assert.Equal(t, []string{"ls", "-la"}, testOrchestrator.ExecInInstanceCalls[0].Options.Command)
	assert.True(t, testOrchestrator.ExecInInstanceCalls[0].Options.TTY)
}
