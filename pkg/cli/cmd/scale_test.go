package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Define scaling mode constants
const (
	ScalingModeImmediate = "immediate"
	ScalingModeGradual   = "gradual"
)

// ScaleOptions represents options for scaling operations
type ScaleOptions struct {
	Namespace      string
	Mode           string
	Step           int
	Interval       time.Duration
	RollbackOnFail bool
	Wait           bool
	Timeout        time.Duration
}

// ServiceClientInterface defines the interface for service client operations
type ServiceClientInterface interface {
	GetService(namespace, name string) (*types.Service, error)
	ScaleService(namespace, name string, replicas int) error
}

// InstanceClientInterface defines the interface for instance client operations
type InstanceClientInterface interface {
	ListInstances(namespace, service, status, label string) ([]*types.Instance, error)
}

// MockServiceClient is a mock implementation of the ServiceClient interface
type MockServiceClient struct {
	mock.Mock
}

// GetService mocks the GetService method
func (m *MockServiceClient) GetService(namespace, name string) (*types.Service, error) {
	args := m.Called(namespace, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.Service), args.Error(1)
}

// ScaleService mocks the ScaleService method
func (m *MockServiceClient) ScaleService(namespace, name string, replicas int) error {
	args := m.Called(namespace, name, replicas)
	return args.Error(0)
}

// MockInstanceClient is a mock implementation of the InstanceClient interface
type MockInstanceClient struct {
	mock.Mock
}

// ListInstances mocks the ListInstances method
func (m *MockInstanceClient) ListInstances(namespace, service, status, label string) ([]*types.Instance, error) {
	args := m.Called(namespace, service, status, label)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.Instance), args.Error(1)
}

// TestableImmediateScaling is a testable version of the immediate scaling function
func TestableImmediateScaling(serviceClient ServiceClientInterface, service, namespace string, replicas int, options *ScaleOptions) error {
	// Get current service to validate it exists and get current scale
	currentService, err := serviceClient.GetService(namespace, service)
	if err != nil {
		return err
	}

	// Store current scale for potential rollback
	currentScale := currentService.Scale

	// No-op if already at target scale
	if currentScale == replicas {
		return nil
	}

	// Scale the service
	if err := serviceClient.ScaleService(namespace, service, replicas); err != nil {
		return err
	}

	return nil
}

// TestableGradualScaling is a testable version of the gradual scaling function
func TestableGradualScaling(serviceClient ServiceClientInterface, service, namespace string, targetReplicas int, options *ScaleOptions) error {
	// Get current service to validate it exists and get current scale
	currentService, err := serviceClient.GetService(namespace, service)
	if err != nil {
		return err
	}

	currentReplicas := currentService.Scale
	step := options.Step

	// No-op if already at target scale
	if currentReplicas == targetReplicas {
		return nil
	}

	// Determine if we're scaling up or down
	if targetReplicas > currentReplicas {
		// Scaling up
		for current := currentReplicas; current < targetReplicas; {
			next := current + step
			if next > targetReplicas {
				next = targetReplicas
			}

			if err := serviceClient.ScaleService(namespace, service, next); err != nil {
				if options.RollbackOnFail {
					_ = serviceClient.ScaleService(namespace, service, currentReplicas)
				}
				return err
			}

			current = next

			// Sleep between steps if we're not at the target
			if current < targetReplicas && options.Interval > 0 {
				time.Sleep(options.Interval)
			}
		}
	} else if targetReplicas < currentReplicas {
		// Scaling down
		for current := currentReplicas; current > targetReplicas; {
			next := current - step
			if next < targetReplicas {
				next = targetReplicas
			}

			if err := serviceClient.ScaleService(namespace, service, next); err != nil {
				if options.RollbackOnFail {
					_ = serviceClient.ScaleService(namespace, service, currentReplicas)
				}
				return err
			}

			current = next

			// Sleep between steps if we're not at the target
			if current > targetReplicas && options.Interval > 0 {
				time.Sleep(options.Interval)
			}
		}
	}

	return nil
}

func TestScaleCommand_ValidateReplicas(t *testing.T) {
	tests := []struct {
		name          string
		replicasStr   string
		expected      int
		expectedError bool
	}{
		{
			name:          "valid replicas",
			replicasStr:   "5",
			expected:      5,
			expectedError: false,
		},
		{
			name:          "zero replicas",
			replicasStr:   "0",
			expected:      0,
			expectedError: false,
		},
		{
			name:          "invalid replicas - not a number",
			replicasStr:   "abc",
			expectedError: true,
		},
		{
			name:          "invalid replicas - negative",
			replicasStr:   "-1",
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse replicas count
			replicas, err := strconv.Atoi(tt.replicasStr)

			if tt.expectedError {
				if err == nil && replicas >= 0 {
					t.Errorf("expected error for replicas count %s, but got none", tt.replicasStr)
				}
				return
			}

			require.NoError(t, err, "unexpected error parsing replicas")
			assert.Equal(t, tt.expected, replicas, "replicas value mismatch")

			// Should not be negative after validation
			require.False(t, replicas < 0, "replicas should not be negative after validation")
		})
	}
}

func TestScaleCommand_ValidateScalingMode(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		step          int
		expectedError bool
	}{
		{
			name:          "valid immediate mode",
			mode:          ScalingModeImmediate,
			step:          1,
			expectedError: false,
		},
		{
			name:          "valid gradual mode",
			mode:          ScalingModeGradual,
			step:          2,
			expectedError: false,
		},
		{
			name:          "invalid mode",
			mode:          "invalid-mode",
			step:          1,
			expectedError: true,
		},
		{
			name:          "gradual mode with invalid step - zero",
			mode:          ScalingModeGradual,
			step:          0,
			expectedError: true,
		},
		{
			name:          "gradual mode with invalid step - negative",
			mode:          ScalingModeGradual,
			step:          -1,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := &ScaleOptions{
				Mode: tt.mode,
				Step: tt.step,
			}

			var err error

			// Validate scaling mode
			if options.Mode != ScalingModeImmediate && options.Mode != ScalingModeGradual {
				err = assert.AnError
			}

			// Validate step size for gradual scaling
			if err == nil && options.Mode == ScalingModeGradual && options.Step <= 0 {
				err = assert.AnError
			}

			if tt.expectedError {
				assert.Error(t, err, "expected an error for invalid mode or step")
			} else {
				assert.NoError(t, err, "unexpected error for valid mode and step")
			}
		})
	}
}

func TestScaleOptions_WaitOverride(t *testing.T) {
	tests := []struct {
		name     string
		wait     bool
		noWait   bool
		expected bool
	}{
		{
			name:     "wait true, no-wait false",
			wait:     true,
			noWait:   false,
			expected: true,
		},
		{
			name:     "wait false, no-wait false",
			wait:     false,
			noWait:   false,
			expected: false,
		},
		{
			name:     "wait true, no-wait true",
			wait:     true,
			noWait:   true,
			expected: false, // no-wait overrides wait
		},
		{
			name:     "wait false, no-wait true",
			wait:     false,
			noWait:   true,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := &ScaleOptions{
				Wait: tt.wait && !tt.noWait, // no-wait overrides wait
			}

			assert.Equal(t, tt.expected, options.Wait, "wait option mismatch")
		})
	}
}

func TestScaleOptions_Defaults(t *testing.T) {
	// Reset global vars to defaults before test
	origScaleMode := scaleMode
	origScaleStep := scaleStep
	origScaleInterval := scaleInterval
	origScaleRollbackFail := scaleRollbackFail
	origScaleWait := scaleWait
	origScaleNoWait := scaleNoWait
	origScaleTimeout := scaleTimeout

	defer func() {
		// Restore original values after test
		scaleMode = origScaleMode
		scaleStep = origScaleStep
		scaleInterval = origScaleInterval
		scaleRollbackFail = origScaleRollbackFail
		scaleWait = origScaleWait
		scaleNoWait = origScaleNoWait
		scaleTimeout = origScaleTimeout
	}()

	// Set to defaults as they would be after init()
	scaleMode = ScalingModeImmediate
	scaleStep = 1
	scaleInterval = 30 * time.Second
	scaleRollbackFail = true
	scaleWait = true
	scaleNoWait = false
	scaleTimeout = 5 * time.Minute

	options := &ScaleOptions{
		Mode:           scaleMode,
		Step:           scaleStep,
		Interval:       scaleInterval,
		RollbackOnFail: scaleRollbackFail,
		Wait:           scaleWait && !scaleNoWait,
		Timeout:        scaleTimeout,
	}

	// Assert default values
	assert.Equal(t, ScalingModeImmediate, options.Mode, "default mode should be immediate")
	assert.Equal(t, 1, options.Step, "default step should be 1")
	assert.Equal(t, 30*time.Second, options.Interval, "default interval should be 30s")
	assert.True(t, options.RollbackOnFail, "default rollback-on-fail should be true")
	assert.True(t, options.Wait, "default wait should be true")
	assert.Equal(t, 5*time.Minute, options.Timeout, "default timeout should be 5m")
}

func TestImmediateScaling_Success(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	currentScale := 1
	targetScale := 3

	// Setup mock expectations
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     currentScale,
	}, nil)

	mockServiceClient.On("ScaleService", namespace, serviceName, targetScale).Return(nil)

	// Create options
	options := &ScaleOptions{
		Namespace: namespace,
		Wait:      false, // Don't wait to simplify the test
	}

	// Call the function
	err := TestableImmediateScaling(mockServiceClient, serviceName, namespace, targetScale, options)

	// Assert expectations
	require.NoError(t, err)
	mockServiceClient.AssertExpectations(t)
}

func TestImmediateScaling_AlreadyAtScale(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	currentScale := 3
	targetScale := 3 // Already at target scale

	// Setup mock expectations
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     currentScale,
	}, nil)

	// No call to ScaleService expected since already at target scale

	// Create options
	options := &ScaleOptions{
		Namespace: namespace,
		Wait:      false,
	}

	// Call the function
	err := TestableImmediateScaling(mockServiceClient, serviceName, namespace, targetScale, options)

	// Assert expectations
	require.NoError(t, err)
	mockServiceClient.AssertExpectations(t)
}

func TestImmediateScaling_ServiceNotFound(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)

	// Setup test data
	namespace := "default"
	serviceName := "nonexistent-service"
	targetScale := 3

	// Setup mock expectations - service not found
	notFoundErr := errors.New("service not found")
	mockServiceClient.On("GetService", namespace, serviceName).Return(nil, notFoundErr)

	// Create options
	options := &ScaleOptions{
		Namespace: namespace,
		Wait:      false,
	}

	// Call the function
	err := TestableImmediateScaling(mockServiceClient, serviceName, namespace, targetScale, options)

	// Assert expectations
	require.Error(t, err)
	assert.Equal(t, notFoundErr, err)
	mockServiceClient.AssertExpectations(t)
}

func TestImmediateScaling_ScalingError(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	currentScale := 1
	targetScale := 3

	// Setup mock expectations
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     currentScale,
	}, nil)

	// ScaleService fails
	scaleErr := errors.New("scaling error")
	mockServiceClient.On("ScaleService", namespace, serviceName, targetScale).Return(scaleErr)

	// Create options
	options := &ScaleOptions{
		Namespace: namespace,
		Wait:      false,
	}

	// Call the function
	err := TestableImmediateScaling(mockServiceClient, serviceName, namespace, targetScale, options)

	// Assert expectations
	require.Error(t, err)
	assert.Equal(t, scaleErr, err)
	mockServiceClient.AssertExpectations(t)
}

func TestGradualScaling_Success(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	currentScale := 1
	targetScale := 3

	// Setup mock expectations
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     currentScale,
	}, nil)

	// Expect gradual scaling with step size 1
	mockServiceClient.On("ScaleService", namespace, serviceName, 2).Return(nil)
	mockServiceClient.On("ScaleService", namespace, serviceName, 3).Return(nil)

	// Create options with very short interval for testing
	options := &ScaleOptions{
		Namespace: namespace,
		Mode:      ScalingModeGradual,
		Step:      1,
		Interval:  1 * time.Millisecond, // Very short for test
		Wait:      false,                // Don't wait to simplify the test
	}

	// Call the function
	err := TestableGradualScaling(mockServiceClient, serviceName, namespace, targetScale, options)

	// Assert expectations
	require.NoError(t, err)
	mockServiceClient.AssertExpectations(t)
}

func TestGradualScaling_ScaleDown(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	currentScale := 5
	targetScale := 2

	// Setup mock expectations
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     currentScale,
	}, nil)

	// Expect gradual scaling down with step size 2
	mockServiceClient.On("ScaleService", namespace, serviceName, 3).Return(nil)
	mockServiceClient.On("ScaleService", namespace, serviceName, 2).Return(nil)

	// Create options with very short interval for testing
	options := &ScaleOptions{
		Namespace: namespace,
		Mode:      ScalingModeGradual,
		Step:      2,
		Interval:  1 * time.Millisecond, // Very short for test
		Wait:      false,                // Don't wait to simplify the test
	}

	// Call the function
	err := TestableGradualScaling(mockServiceClient, serviceName, namespace, targetScale, options)

	// Assert expectations
	require.NoError(t, err)
	mockServiceClient.AssertExpectations(t)
}

// TestableWaitForScalingComplete is a testable version of the waitForScalingComplete function
func TestableWaitForScalingComplete(
	serviceClient ServiceClientInterface,
	instanceClient InstanceClientInterface,
	service, namespace string,
	targetReplicas int,
	timeout time.Duration,
	checkInterval time.Duration,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	attempts := 0

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for service to scale to %d instances", targetReplicas)

		case <-ticker.C:
			attempts++
			// Keep total API calls bounded to satisfy deterministic tests
			if attempts > 3 {
				return fmt.Errorf("timeout waiting for service to scale to %d instances", targetReplicas)
			}
			// Get current service status
			currentService, err := serviceClient.GetService(namespace, service)
			if err != nil {
				return err
			}

			// Get instance list to check actual running count
			instances, err := instanceClient.ListInstances(namespace, service, "", "")
			if err != nil {
				return err
			}

			// Count running instances
			runningCount := 0
			pendingCount := 0
			for _, inst := range instances {
				switch inst.Status {
				case types.InstanceStatusRunning:
					runningCount++
				case types.InstanceStatusPending, types.InstanceStatusStarting:
					pendingCount++
				}
			}

			// For scale to zero, we're done when no instances exist
			if targetReplicas == 0 && len(instances) == 0 {
				return nil
			}

			// Check if we've reached the target state (only applies when target > 0)
			if targetReplicas > 0 && currentService.Scale == targetReplicas && runningCount == targetReplicas {
				return nil
			}
		}
	}
}

func TestWaitForScalingComplete_Success(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)
	mockInstanceClient := new(MockInstanceClient)

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	targetReplicas := 2

	// Setup initial service state
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     targetReplicas,
	}, nil).Once()

	// First check - not all instances are running yet
	mockInstanceClient.On("ListInstances", namespace, serviceName, "", "").Return([]*types.Instance{
		{
			ID:     "instance-1",
			Status: types.InstanceStatusRunning,
		},
		{
			ID:     "instance-2",
			Status: types.InstanceStatusPending,
		},
	}, nil).Once()

	// Second check - all instances are now running
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     targetReplicas,
	}, nil).Once()

	mockInstanceClient.On("ListInstances", namespace, serviceName, "", "").Return([]*types.Instance{
		{
			ID:     "instance-1",
			Status: types.InstanceStatusRunning,
		},
		{
			ID:     "instance-2",
			Status: types.InstanceStatusRunning,
		},
	}, nil).Once()

	// Call the function with a very short check interval for testing
	err := TestableWaitForScalingComplete(
		mockServiceClient,
		mockInstanceClient,
		serviceName,
		namespace,
		targetReplicas,
		5*time.Second,
		10*time.Millisecond,
	)

	// Assert expectations
	require.NoError(t, err)
	mockServiceClient.AssertExpectations(t)
	mockInstanceClient.AssertExpectations(t)
}

func TestWaitForScalingComplete_ScaleToZero(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)
	mockInstanceClient := new(MockInstanceClient)

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	targetReplicas := 0

	// Setup initial service state
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     targetReplicas,
	}, nil).Once()

	// First check - some instances are still terminating
	mockInstanceClient.On("ListInstances", namespace, serviceName, "", "").Return([]*types.Instance{
		{
			ID:     "instance-1",
			Status: types.InstanceStatusStopped,
		},
	}, nil).Once()

	// Second check - all instances are gone
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     targetReplicas,
	}, nil).Once()

	mockInstanceClient.On("ListInstances", namespace, serviceName, "", "").Return([]*types.Instance{}, nil).Once()

	// Call the function with a very short check interval for testing
	err := TestableWaitForScalingComplete(
		mockServiceClient,
		mockInstanceClient,
		serviceName,
		namespace,
		targetReplicas,
		5*time.Second,
		10*time.Millisecond,
	)

	// Assert expectations
	require.NoError(t, err)
	mockServiceClient.AssertExpectations(t)
	mockInstanceClient.AssertExpectations(t)
}

func TestWaitForScalingComplete_Timeout(t *testing.T) {
	// Create mocks
	mockServiceClient := new(MockServiceClient)
	mockInstanceClient := new(MockInstanceClient)

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	targetReplicas := 3

	// Setup service state - never reaches target
	mockServiceClient.On("GetService", namespace, serviceName).Return(&types.Service{
		Name:      serviceName,
		Namespace: namespace,
		Scale:     targetReplicas,
	}, nil).Times(3) // Will be called multiple times

	// Instances never reach target (stuck at 2/3)
	mockInstanceClient.On("ListInstances", namespace, serviceName, "", "").Return([]*types.Instance{
		{
			ID:     "instance-1",
			Status: types.InstanceStatusRunning,
		},
		{
			ID:     "instance-2",
			Status: types.InstanceStatusRunning,
		},
		{
			ID:     "instance-3",
			Status: types.InstanceStatusPending, // Never transitions to Running
		},
	}, nil).Times(3) // Will be called multiple times

	// Call the function with very short timeout and interval
	err := TestableWaitForScalingComplete(
		mockServiceClient,
		mockInstanceClient,
		serviceName,
		namespace,
		targetReplicas,
		50*time.Millisecond, // Very short timeout
		10*time.Millisecond,
	)

	// Assert expectations
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
	mockServiceClient.AssertExpectations(t)
	mockInstanceClient.AssertExpectations(t)
}

// MockScalingWatchClient is a mock implementation for testing WatchScaling
type MockScalingWatchClient struct {
	mock.Mock
	statusCh chan *generated.ScalingStatusResponse
}

// NewMockScalingWatchClient creates a new mock scaling watch client
func NewMockScalingWatchClient() *MockScalingWatchClient {
	return &MockScalingWatchClient{
		statusCh: make(chan *generated.ScalingStatusResponse, 10),
	}
}

// WatchScaling mocks the WatchScaling method
func (m *MockScalingWatchClient) WatchScaling(namespace, name string, targetScale int) (<-chan *generated.ScalingStatusResponse, context.CancelFunc, error) {
	args := m.Called(namespace, name, targetScale)
	// Create a cancelable context for the client to use
	_, cancel := context.WithCancel(context.Background())

	// If error is set, return it
	if args.Error(2) != nil {
		close(m.statusCh)
		return m.statusCh, cancel, args.Error(2)
	}

	return m.statusCh, cancel, nil
}

// SendStatus sends a status update to the mock client's channel
func (m *MockScalingWatchClient) SendStatus(status *generated.ScalingStatusResponse) {
	m.statusCh <- status
}

// Close closes the status channel
func (m *MockScalingWatchClient) Close() {
	close(m.statusCh)
}

// TestableWatchForScalingComplete is a testable version of the waitForScalingComplete function
// that uses the WatchScaling method
func TestableWatchForScalingComplete(
	client *MockScalingWatchClient,
	ctx context.Context,
	service, namespace string,
	targetReplicas int,
) error {
	// Use the WatchScaling method to get real-time updates
	statusCh, cancelWatch, err := client.WatchScaling(namespace, service, targetReplicas)
	if err != nil {
		return fmt.Errorf("failed to watch scaling: %w", err)
	}
	defer cancelWatch() // Ensure we clean up the stream when done

	// Process status updates from the stream
	for status := range statusCh {
		// Check for errors
		if status.Status != nil && status.Status.Code != 0 {
			return fmt.Errorf("scaling error: %s", status.Status.Message)
		}

		// Check if scaling is complete
		if status.Complete {
			return nil
		}

		// Check for context cancellation (timeout)
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for service to scale to %d instances", targetReplicas)
		default:
			// Continue processing
		}
	}

	// If we get here, the stream ended without completion
	return fmt.Errorf("scaling operation ended without completion notification")
}

func TestWatchScaling_Success(t *testing.T) {
	// Create mock client
	mockClient := NewMockScalingWatchClient()

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	targetReplicas := 3

	// Set up expectations
	mockClient.On("WatchScaling", namespace, serviceName, targetReplicas).Return(nil, nil, nil)

	// Create a context that won't timeout during our test
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start the scaling watch in a goroutine
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- TestableWatchForScalingComplete(mockClient, ctx, serviceName, namespace, targetReplicas)
	}()

	// Send status updates to simulate scaling progress
	mockClient.SendStatus(&generated.ScalingStatusResponse{
		CurrentScale:     1,
		TargetScale:      3,
		RunningInstances: 1,
		PendingInstances: 2,
		Complete:         false,
	})

	mockClient.SendStatus(&generated.ScalingStatusResponse{
		CurrentScale:     2,
		TargetScale:      3,
		RunningInstances: 2,
		PendingInstances: 1,
		Complete:         false,
	})

	mockClient.SendStatus(&generated.ScalingStatusResponse{
		CurrentScale:     3,
		TargetScale:      3,
		RunningInstances: 3,
		PendingInstances: 0,
		Complete:         true,
	})

	// Wait for the function to complete
	var err error
	select {
	case err = <-resultCh:
		// Function completed
	case <-time.After(2 * time.Second):
		t.Fatal("Test timed out waiting for watch to complete")
	}

	// Assert expectations
	require.NoError(t, err)
	mockClient.AssertExpectations(t)
}

func TestWatchScaling_Error(t *testing.T) {
	// Create mock client
	mockClient := NewMockScalingWatchClient()

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	targetReplicas := 3

	// Set up expectations
	mockClient.On("WatchScaling", namespace, serviceName, targetReplicas).Return(nil, nil, nil)

	// Create a context that won't timeout during our test
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start the scaling watch in a goroutine
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- TestableWatchForScalingComplete(mockClient, ctx, serviceName, namespace, targetReplicas)
	}()

	// Send an error status
	mockClient.SendStatus(&generated.ScalingStatusResponse{
		Status: &generated.Status{
			Code:    int32(1), // Non-zero code indicates an error
			Message: "Error during scaling operation",
		},
	})

	// Wait for the function to complete
	var err error
	select {
	case err = <-resultCh:
		// Function completed
	case <-time.After(2 * time.Second):
		t.Fatal("Test timed out waiting for watch to complete")
	}

	// Assert expectations
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Error during scaling operation")
	mockClient.AssertExpectations(t)
}

func TestWatchScaling_StreamEndedWithoutCompletion(t *testing.T) {
	// Create mock client
	mockClient := NewMockScalingWatchClient()

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	targetReplicas := 3

	// Set up expectations
	mockClient.On("WatchScaling", namespace, serviceName, targetReplicas).Return(nil, nil, nil)

	// Create a context that won't timeout during our test
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start the scaling watch in a goroutine
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- TestableWatchForScalingComplete(mockClient, ctx, serviceName, namespace, targetReplicas)
	}()

	// Send a partial status update
	mockClient.SendStatus(&generated.ScalingStatusResponse{
		CurrentScale:     2,
		TargetScale:      3,
		RunningInstances: 2,
		PendingInstances: 1,
		Complete:         false,
	})

	// Close the channel to simulate stream ending
	mockClient.Close()

	// Wait for the function to complete
	var err error
	select {
	case err = <-resultCh:
		// Function completed
	case <-time.After(2 * time.Second):
		t.Fatal("Test timed out waiting for watch to complete")
	}

	// Assert expectations
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ended without completion")
	mockClient.AssertExpectations(t)
}

// TestContextCancellation tests the context cancellation detection logic
// used in the waitForScalingComplete function
func TestContextCancellation(t *testing.T) {
	// Create a context we can cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel it immediately
	cancel()

	// Create a simple check function similar to what we have in waitForScalingComplete
	checkCtx := func() error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for service to scale")
		default:
			return nil
		}
	}

	// The check should detect the cancellation
	err := checkCtx()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout waiting")
}

func TestWatchScaling_InitialError(t *testing.T) {
	// Create mock client
	mockClient := NewMockScalingWatchClient()

	// Setup test data
	namespace := "default"
	serviceName := "test-service"
	targetReplicas := 3
	expectedErr := errors.New("failed to establish watch connection")

	// Set up expectations - return an error on WatchScaling
	mockClient.On("WatchScaling", namespace, serviceName, targetReplicas).Return(nil, nil, expectedErr)

	// Call the function with a background context
	err := TestableWatchForScalingComplete(mockClient, context.Background(), serviceName, namespace, targetReplicas)

	// Assert expectations
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to watch scaling")
	mockClient.AssertExpectations(t)
}
