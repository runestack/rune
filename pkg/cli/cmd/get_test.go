package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/utils"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestResolveResourceType(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expected      string
		expectedError bool
	}{
		{"service full name", "service", "service", false},
		{"services plural", "services", "service", false},
		{"service abbreviation", "svc", "service", false},
		{"instance full name", "instance", "instance", false},
		{"instances plural", "instances", "instance", false},
		{"instance abbreviation", "inst", "instance", false},
		{"namespace full name", "namespace", "namespace", false},
		{"namespaces plural", "namespaces", "namespace", false},
		{"namespace abbreviation", "ns", "namespace", false},
		{"job full name", "job", "job", false},
		{"jobs plural", "jobs", "job", false},
		{"config full name", "config", "config", false},
		{"configs plural", "configs", "config", false},
		{"config abbreviation", "cfg", "config", false},
		{"secret full name", "secret", "secret", false},
		{"secrets plural", "secrets", "secret", false},
		{"invalid resource type", "unknown", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := resolveResourceType(tc.input)
			if tc.expectedError {
				assert.Error(t, err)
				assert.Empty(t, result)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestParseSelector(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expected      map[string]string
		expectedError bool
	}{
		{"empty", "", map[string]string{}, false},
		{"single key-value", "app=frontend", map[string]string{"app": "frontend"}, false},
		{"multiple key-values", "app=frontend,env=prod", map[string]string{"app": "frontend", "env": "prod"}, false},
		{"with spaces", " app = frontend , env = prod ", map[string]string{"app": "frontend", "env": "prod"}, false},
		{"empty key", "=value", nil, true},
		{"missing value", "app=", nil, true},
		{"missing equals", "app", nil, true},
		{"double equals", "app==value", nil, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := utils.ParseSelector(tc.input)
			if tc.expectedError {
				assert.Error(t, err)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

// TestSortServices tests the sortServices function
func TestSortServices(t *testing.T) {
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)
	lastWeek := now.Add(-7 * 24 * time.Hour)

	services := []*types.Service{
		{
			Name:     "zebra",
			Status:   types.ServiceStatusRunning,
			Metadata: &types.ServiceMetadata{CreatedAt: lastWeek},
		},
		{
			Name:     "apple",
			Status:   types.ServiceStatusPending,
			Metadata: &types.ServiceMetadata{CreatedAt: yesterday},
		},
		{
			Name:     "banana",
			Status:   types.ServiceStatusFailed,
			Metadata: &types.ServiceMetadata{CreatedAt: now},
		},
	}

	// Test sorting by name
	t.Run("sort by name", func(t *testing.T) {
		svcs := make([]*types.Service, len(services))
		copy(svcs, services)
		sortServices(svcs, "name")
		assert.Equal(t, "apple", svcs[0].Name)
		assert.Equal(t, "banana", svcs[1].Name)
		assert.Equal(t, "zebra", svcs[2].Name)
	})

	// Test sorting by age
	t.Run("sort by age", func(t *testing.T) {
		svcs := make([]*types.Service, len(services))
		copy(svcs, services)
		sortServices(svcs, "age")
		assert.Equal(t, "zebra", svcs[0].Name) // oldest first
		assert.Equal(t, "apple", svcs[1].Name)
		assert.Equal(t, "banana", svcs[2].Name) // newest last
	})

	// Test sorting by status
	t.Run("sort by status", func(t *testing.T) {
		svcs := make([]*types.Service, len(services))
		copy(svcs, services)
		sortServices(svcs, "status")
		assert.Equal(t, string(types.ServiceStatusFailed), string(svcs[0].Status))
		assert.Equal(t, string(types.ServiceStatusPending), string(svcs[1].Status))
		assert.Equal(t, string(types.ServiceStatusRunning), string(svcs[2].Status))
	})
}

// This mock implementation would work with any tests that call ListServices
// but we don't currently have direct tests for handleServiceGet that
// would need updating. Tests that actually use the serviceClient should
// be updated separately as needed.

// What's most important is that the parseSelector function tests
// continue to work, and that sortServices tests work, as these are
// still used on the client side.

// TestWatchServices tests the service watching functionality
func TestWatchServices(t *testing.T) {
	// Mock service client
	mockClient := &mockServiceClient{}

	// Create a context with timeout for the test
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Test cases for different watch scenarios
	tests := []struct {
		name          string
		resourceName  string
		namespace     string
		watchEvents   []client.WatchEvent
		expectedError bool
	}{
		{
			name:         "Watch single service",
			resourceName: "test-service",
			namespace:    "default",
			watchEvents: []client.WatchEvent{
				{
					Service: &types.Service{
						ID:        "svc-1",
						Name:      "test-service",
						Namespace: "default",
						Image:     "test:latest",
						Scale:     1,
						Status:    types.ServiceStatusRunning,
						Metadata:  &types.ServiceMetadata{CreatedAt: time.Now().Add(-1 * time.Hour)},
					},
					EventType: "ADDED",
					Error:     nil,
				},
				{
					Service: &types.Service{
						ID:        "svc-1",
						Name:      "test-service",
						Namespace: "default",
						Image:     "test:latest",
						Scale:     2, // Scale changed
						Status:    types.ServiceStatusDeploying,
						Metadata:  &types.ServiceMetadata{CreatedAt: time.Now().Add(-1 * time.Hour)},
					},
					EventType: "MODIFIED",
					Error:     nil,
				},
			},
			expectedError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up test globals
			opts := &getOptions{}
			opts.namespace = tt.namespace
			opts.allNamespaces = tt.namespace == "*"

			// Create a buffered watch channel
			watchCh := make(chan client.WatchEvent, len(tt.watchEvents))

			// Fill the channel with test events
			for _, event := range tt.watchEvents {
				watchCh <- event
			}

			// Set up the mock client to return our channel
			mockClient.watchCh = watchCh
			mockClient.watchErr = nil

			// Start the watch in a goroutine
			errCh := make(chan error, 1)
			go func() {
				// Close the channel after our test events to simulate end of stream
				defer close(watchCh)
				errCh <- watchServices(ctx, mockClient, tt.resourceName, opts)
			}()

			// Wait for the function to complete or timeout
			select {
			case err := <-errCh:
				if tt.expectedError && err == nil {
					t.Errorf("Expected error but got nil")
				}
				if !tt.expectedError && err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Errorf("Test timed out")
			}
		})
	}
}

// mockServiceClient is a mock implementation of the ServiceClient for testing
type mockServiceClient struct {
	watchCh  chan client.WatchEvent
	watchErr error
}

// WatchServices implements the ServiceClient.WatchServices method for testing
func (m *mockServiceClient) WatchServices(namespace, labelSelector, fieldSelector string) (<-chan client.WatchEvent, context.CancelFunc, error) {
	if m.watchErr != nil {
		return nil, nil, m.watchErr
	}
	// Return a no-op cancel function for testing
	cancelFunc := func() {}
	return m.watchCh, cancelFunc, nil
}
