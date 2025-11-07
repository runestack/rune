package repos

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

func TestServiceRepo_Create(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewServiceRepo(testStore)

	service := &types.Service{
		ID:        "test-service-123",
		Name:      "test-service",
		Namespace: "default",
		Image:     "nginx:latest",
		Scale:     2,
		Status:    types.ServiceStatusPending,
	}

	err := repo.Create(ctx, service)
	if err != nil {
		t.Fatalf("Failed to create service: %v", err)
	}

	// Verify the service was created with proper metadata
	if service.Metadata == nil {
		t.Fatal("Service metadata should not be nil")
	}
	if service.Metadata.CreatedAt.IsZero() {
		t.Fatal("Service CreatedAt should not be zero")
	}
	if service.Metadata.UpdatedAt.IsZero() {
		t.Fatal("Service UpdatedAt should not be zero")
	}
	if service.Metadata.Generation != 1 {
		t.Fatalf("Service Generation should be 1, got %d", service.Metadata.Generation)
	}
}

func TestServiceRepo_CreateRef(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewServiceRepo(testStore)

	service := &types.Service{
		ID:     "test-service-123",
		Image:  "nginx:latest",
		Scale:  2,
		Status: types.ServiceStatusPending,
	}

	err := repo.CreateRef(ctx, "service:test-service.default.rune", service)
	if err != nil {
		t.Fatalf("Failed to create service with ref: %v", err)
	}

	if service.Namespace != "default" {
		t.Fatalf("Expected namespace 'default', got '%s'", service.Namespace)
	}
	if service.Name != "test-service" {
		t.Fatalf("Expected name 'test-service', got '%s'", service.Name)
	}
}

func TestServiceRepo_Get(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewServiceRepo(testStore)

	// Create a service first
	service := &types.Service{
		ID:        "test-service-123",
		Name:      "test-service",
		Namespace: "default",
		Image:     "nginx:latest",
		Scale:     2,
		Status:    types.ServiceStatusPending,
	}

	err := repo.Create(ctx, service)
	if err != nil {
		t.Fatalf("Failed to create service: %v", err)
	}

	// Retrieve the service
	retrieved, err := repo.Get(ctx, "service:test-service.default.rune")
	if err != nil {
		t.Fatalf("Failed to get service: %v", err)
	}

	if retrieved.Name != "test-service" {
		t.Fatalf("Expected name 'test-service', got '%s'", retrieved.Name)
	}
	if retrieved.Namespace != "default" {
		t.Fatalf("Expected namespace 'default', got '%s'", retrieved.Namespace)
	}
	if retrieved.Image != "nginx:latest" {
		t.Fatalf("Expected image 'nginx:latest', got '%s'", retrieved.Image)
	}
}

func TestServiceRepo_Update(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewServiceRepo(testStore)

	// Create a service first
	service := &types.Service{
		ID:        "test-service-123",
		Name:      "test-service",
		Namespace: "default",
		Image:     "nginx:latest",
		Scale:     2,
		Status:    types.ServiceStatusPending,
	}

	err := repo.Create(ctx, service)
	if err != nil {
		t.Fatalf("Failed to create service: %v", err)
	}

	originalGeneration := service.Metadata.Generation

	// Update the service
	service.Scale = 3
	err = repo.Update(ctx, "service:test-service.default.rune", service)
	if err != nil {
		t.Fatalf("Failed to update service: %v", err)
	}

	if service.Metadata.Generation != originalGeneration+1 {
		t.Fatalf("Expected generation %d, got %d", originalGeneration+1, service.Metadata.Generation)
	}
}

func TestServiceRepo_List(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewServiceRepo(testStore)

	// Create multiple services
	services := []*types.Service{
		{
			ID:        "service-1-123",
			Name:      "service-1",
			Namespace: "default",
			Image:     "nginx:latest",
			Scale:     1,
		},
		{
			ID:        "service-2-123",
			Name:      "service-2",
			Namespace: "default",
			Image:     "redis:latest",
			Scale:     1,
		},
	}

	for _, service := range services {
		err := repo.Create(ctx, service)
		if err != nil {
			t.Fatalf("Failed to create service %s: %v", service.Name, err)
		}
	}

	// List services
	list, err := repo.List(ctx, "default")
	if err != nil {
		t.Fatalf("Failed to list services: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("Expected 2 services, got %d", len(list))
	}
}

func TestServiceRepo_UpdateStatus(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewServiceRepo(testStore)

	// Create a service first
	service := &types.Service{
		ID:        "test-service-123",
		Name:      "test-service",
		Namespace: "default",
		Image:     "nginx:latest",
		Scale:     2,
		Status:    types.ServiceStatusPending,
	}

	err := repo.Create(ctx, service)
	if err != nil {
		t.Fatalf("Failed to create service: %v", err)
	}

	// Update status
	err = repo.UpdateStatus(ctx, "service:test-service.default.rune", types.ServiceStatusRunning)
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}

	// Verify the status was updated
	retrieved, err := repo.Get(ctx, "service:test-service.default.rune")
	if err != nil {
		t.Fatalf("Failed to get service: %v", err)
	}

	if retrieved.Status != types.ServiceStatusRunning {
		t.Fatalf("Expected status %s, got %s", types.ServiceStatusRunning, retrieved.Status)
	}
}

func TestServiceRepo_UpdateScale(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	repo := NewServiceRepo(testStore)

	// Create a service first
	service := &types.Service{
		ID:        "test-service-123",
		Name:      "test-service",
		Namespace: "default",
		Image:     "nginx:latest",
		Scale:     2,
		Status:    types.ServiceStatusPending,
	}

	err := repo.Create(ctx, service)
	if err != nil {
		t.Fatalf("Failed to create service: %v", err)
	}

	// Update scale
	err = repo.UpdateScale(ctx, "service:test-service.default.rune", 5)
	if err != nil {
		t.Fatalf("Failed to update scale: %v", err)
	}

	// Verify the scale was updated
	retrieved, err := repo.Get(ctx, "service:test-service.default.rune")
	if err != nil {
		t.Fatalf("Failed to get service: %v", err)
	}

	if retrieved.Scale != 5 {
		t.Fatalf("Expected scale 5, got %d", retrieved.Scale)
	}
	if retrieved.Metadata.LastNonZeroScale != 5 {
		t.Fatalf("Expected LastNonZeroScale 5, got %d", retrieved.Metadata.LastNonZeroScale)
	}
}
