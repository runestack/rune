package repos

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

func TestInstanceRepo_Create(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	instance := &types.Instance{
		ID:          "test-instance-123",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service-456",
		ServiceName: "test-service",
		NodeID:      "node-1",
		Status:      types.InstanceStatusPending,
		Runner:      types.RunnerTypeDocker,
	}

	err := repo.Create(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Verify the instance was created with proper timestamps
	if instance.CreatedAt.IsZero() {
		t.Fatal("Instance CreatedAt should not be zero")
	}
	if instance.UpdatedAt.IsZero() {
		t.Fatal("Instance UpdatedAt should not be zero")
	}
}

func TestInstanceRepo_CreateRef(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	instance := &types.Instance{
		ID:          "test-instance-123",
		ServiceID:   "test-service-456",
		ServiceName: "test-service",
		NodeID:      "node-1",
		Status:      types.InstanceStatusPending,
		Runner:      types.RunnerTypeDocker,
	}

	err := repo.CreateRef(ctx, "instance:test-instance-123.default.rune", instance)
	if err != nil {
		t.Fatalf("Failed to create instance with ref: %v", err)
	}

	if instance.Namespace != "default" {
		t.Fatalf("Expected namespace 'default', got '%s'", instance.Namespace)
	}
	if instance.Name != "test-instance-123" {
		t.Fatalf("Expected name 'test-instance-123', got '%s'", instance.Name)
	}
}

func TestInstanceRepo_Get(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create an instance first
	instance := &types.Instance{
		ID:          "test-instance-123",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service-456",
		ServiceName: "test-service",
		NodeID:      "node-1",
		Status:      types.InstanceStatusPending,
		Runner:      types.RunnerTypeDocker,
	}

	err := repo.Create(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Retrieve the instance
	retrieved, err := repo.Get(ctx, "instance:test-instance-123.default.rune")
	if err != nil {
		t.Fatalf("Failed to get instance: %v", err)
	}

	if retrieved.ID != "test-instance-123" {
		t.Fatalf("Expected ID 'test-instance-123', got '%s'", retrieved.ID)
	}
	if retrieved.Name != "test-instance" {
		t.Fatalf("Expected name 'test-instance', got '%s'", retrieved.Name)
	}
	if retrieved.ServiceID != "test-service-456" {
		t.Fatalf("Expected ServiceID 'test-service-456', got '%s'", retrieved.ServiceID)
	}
}

func TestInstanceRepo_GetByInstanceID(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create an instance first
	instance := &types.Instance{
		ID:          "test-instance-123",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service-456",
		ServiceName: "test-service",
		NodeID:      "node-1",
		Status:      types.InstanceStatusPending,
		Runner:      types.RunnerTypeDocker,
	}

	err := repo.Create(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Retrieve the instance by ID
	retrieved, err := repo.GetByInstanceID(ctx, "default", "test-instance-123")
	if err != nil {
		t.Fatalf("Failed to get instance by ID: %v", err)
	}

	if retrieved.ID != "test-instance-123" {
		t.Fatalf("Expected ID 'test-instance-123', got '%s'", retrieved.ID)
	}
}

func TestInstanceRepo_Update(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create an instance first
	instance := &types.Instance{
		ID:          "test-instance-123",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service-456",
		ServiceName: "test-service",
		NodeID:      "node-1",
		Status:      types.InstanceStatusPending,
		Runner:      types.RunnerTypeDocker,
	}

	err := repo.Create(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	originalUpdatedAt := instance.UpdatedAt

	// Update the instance
	instance.Status = types.InstanceStatusRunning
	err = repo.Update(ctx, "instance:test-instance-123.default.rune", instance)
	if err != nil {
		t.Fatalf("Failed to update instance: %v", err)
	}

	if instance.UpdatedAt.Equal(originalUpdatedAt) {
		t.Fatal("Instance UpdatedAt should have changed")
	}
}

func TestInstanceRepo_List(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create multiple instances
	instances := []*types.Instance{
		{
			ID:          "instance-1",
			Name:        "instance-1",
			Namespace:   "default",
			ServiceID:   "service-1",
			ServiceName: "service-1",
			NodeID:      "node-1",
			Status:      types.InstanceStatusPending,
			Runner:      types.RunnerTypeDocker,
		},
		{
			ID:          "instance-2",
			Name:        "instance-2",
			Namespace:   "default",
			ServiceID:   "service-2",
			ServiceName: "service-2",
			NodeID:      "node-2",
			Status:      types.InstanceStatusPending,
			Runner:      types.RunnerTypeDocker,
		},
	}

	for _, instance := range instances {
		err := repo.Create(ctx, instance)
		if err != nil {
			t.Fatalf("Failed to create instance %s: %v", instance.Name, err)
		}
	}

	// List instances
	list, err := repo.List(ctx, "default")
	if err != nil {
		t.Fatalf("Failed to list instances: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("Expected 2 instances, got %d", len(list))
	}
}

func TestInstanceRepo_ListByService(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create instances for different services
	instances := []*types.Instance{
		{
			ID:          "instance-1",
			Name:        "instance-1",
			Namespace:   "default",
			ServiceID:   "service-1",
			ServiceName: "service-1",
			NodeID:      "node-1",
			Status:      types.InstanceStatusPending,
			Runner:      types.RunnerTypeDocker,
		},
		{
			ID:          "instance-2",
			Name:        "instance-2",
			Namespace:   "default",
			ServiceID:   "service-1", // Same service
			ServiceName: "service-1",
			NodeID:      "node-2",
			Status:      types.InstanceStatusPending,
			Runner:      types.RunnerTypeDocker,
		},
		{
			ID:          "instance-3",
			Name:        "instance-3",
			Namespace:   "default",
			ServiceID:   "service-2", // Different service
			ServiceName: "service-2",
			NodeID:      "node-3",
			Status:      types.InstanceStatusPending,
			Runner:      types.RunnerTypeDocker,
		},
	}

	for _, instance := range instances {
		err := repo.Create(ctx, instance)
		if err != nil {
			t.Fatalf("Failed to create instance %s: %v", instance.Name, err)
		}
	}

	// List instances by service
	list, err := repo.ListByService(ctx, "default", "service-1")
	if err != nil {
		t.Fatalf("Failed to list instances by service: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("Expected 2 instances for service-1, got %d", len(list))
	}
}

func TestInstanceRepo_ListByStatus(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create instances with different statuses
	instances := []*types.Instance{
		{
			ID:          "instance-1",
			Name:        "instance-1",
			Namespace:   "default",
			ServiceID:   "service-1",
			ServiceName: "service-1",
			NodeID:      "node-1",
			Status:      types.InstanceStatusRunning,
			Runner:      types.RunnerTypeDocker,
		},
		{
			ID:          "instance-2",
			Name:        "instance-2",
			Namespace:   "default",
			ServiceID:   "service-1",
			ServiceName: "service-1",
			NodeID:      "node-2",
			Status:      types.InstanceStatusRunning, // Same status
			Runner:      types.RunnerTypeDocker,
		},
		{
			ID:          "instance-3",
			Name:        "instance-3",
			Namespace:   "default",
			ServiceID:   "service-2",
			ServiceName: "service-2",
			NodeID:      "node-3",
			Status:      types.InstanceStatusPending, // Different status
			Runner:      types.RunnerTypeDocker,
		},
	}

	for _, instance := range instances {
		err := repo.Create(ctx, instance)
		if err != nil {
			t.Fatalf("Failed to create instance %s: %v", instance.Name, err)
		}
	}

	// List instances by status
	list, err := repo.ListByStatus(ctx, "default", types.InstanceStatusRunning)
	if err != nil {
		t.Fatalf("Failed to list instances by status: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("Expected 2 running instances, got %d", len(list))
	}
}

func TestInstanceRepo_UpdateStatus(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create an instance first
	instance := &types.Instance{
		ID:          "test-instance-123",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service-456",
		ServiceName: "test-service",
		NodeID:      "node-1",
		Status:      types.InstanceStatusPending,
		Runner:      types.RunnerTypeDocker,
	}

	err := repo.Create(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Update status
	err = repo.UpdateStatus(ctx, "instance:test-instance-123.default.rune", types.InstanceStatusRunning, "Instance is now running")
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}

	// Verify the status was updated
	retrieved, err := repo.Get(ctx, "instance:test-instance-123.default.rune")
	if err != nil {
		t.Fatalf("Failed to get instance: %v", err)
	}

	if retrieved.Status != types.InstanceStatusRunning {
		t.Fatalf("Expected status %s, got %s", types.InstanceStatusRunning, retrieved.Status)
	}
	if retrieved.StatusMessage != "Instance is now running" {
		t.Fatalf("Expected status message 'Instance is now running', got '%s'", retrieved.StatusMessage)
	}
}

func TestInstanceRepo_UpdateIP(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create an instance first
	instance := &types.Instance{
		ID:          "test-instance-123",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service-456",
		ServiceName: "test-service",
		NodeID:      "node-1",
		Status:      types.InstanceStatusPending,
		Runner:      types.RunnerTypeDocker,
		IP:          "10.0.0.1",
	}

	err := repo.Create(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Update IP
	err = repo.UpdateIP(ctx, "instance:test-instance-123.default.rune", "10.0.0.2")
	if err != nil {
		t.Fatalf("Failed to update IP: %v", err)
	}

	// Verify the IP was updated
	retrieved, err := repo.Get(ctx, "instance:test-instance-123.default.rune")
	if err != nil {
		t.Fatalf("Failed to get instance: %v", err)
	}

	if retrieved.IP != "10.0.0.2" {
		t.Fatalf("Expected IP '10.0.0.2', got '%s'", retrieved.IP)
	}
}

func TestInstanceRepo_IncrementRestartCount(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create an instance first
	instance := &types.Instance{
		ID:          "test-instance-123",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service-456",
		ServiceName: "test-service",
		NodeID:      "node-1",
		Status:      types.InstanceStatusPending,
		Runner:      types.RunnerTypeDocker,
	}

	err := repo.Create(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Increment restart count
	err = repo.IncrementRestartCount(ctx, "instance:test-instance-123.default.rune")
	if err != nil {
		t.Fatalf("Failed to increment restart count: %v", err)
	}

	// Verify the restart count was incremented
	retrieved, err := repo.Get(ctx, "instance:test-instance-123.default.rune")
	if err != nil {
		t.Fatalf("Failed to get instance: %v", err)
	}

	if retrieved.Metadata.RestartCount != 1 {
		t.Fatalf("Expected restart count 1, got %d", retrieved.Metadata.RestartCount)
	}
}

func TestInstanceRepo_CountByService(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewInstanceRepo(testStore)

	// Create instances for different services
	instances := []*types.Instance{
		{
			ID:          "instance-1",
			Name:        "instance-1",
			Namespace:   "default",
			ServiceID:   "service-1",
			ServiceName: "service-1",
			NodeID:      "node-1",
			Status:      types.InstanceStatusPending,
			Runner:      types.RunnerTypeDocker,
		},
		{
			ID:          "instance-2",
			Name:        "instance-2",
			Namespace:   "default",
			ServiceID:   "service-1", // Same service
			ServiceName: "service-1",
			NodeID:      "node-2",
			Status:      types.InstanceStatusPending,
			Runner:      types.RunnerTypeDocker,
		},
		{
			ID:          "instance-3",
			Name:        "instance-3",
			Namespace:   "default",
			ServiceID:   "service-2", // Different service
			ServiceName: "service-2",
			NodeID:      "node-3",
			Status:      types.InstanceStatusPending,
			Runner:      types.RunnerTypeDocker,
		},
	}

	for _, instance := range instances {
		err := repo.Create(ctx, instance)
		if err != nil {
			t.Fatalf("Failed to create instance %s: %v", instance.Name, err)
		}
	}

	// Count instances by service
	count, err := repo.CountByService(ctx, "default", "service-1")
	if err != nil {
		t.Fatalf("Failed to count instances by service: %v", err)
	}

	if count != 2 {
		t.Fatalf("Expected 2 instances for service-1, got %d", count)
	}
}
