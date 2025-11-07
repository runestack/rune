package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// createTempDir creates a temporary directory for testing.
func createTempDir() (string, error) {
	return os.MkdirTemp("", "badger-test")
}

// setupTestStore creates a test BadgerDB store with a temporary directory.
func setupTestStore(t *testing.T) (*BadgerStore, string, func()) {
	dir, err := createTempDir()
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}

	logger := log.NewTestLogger()
	store := NewBadgerStore(logger)
	err = store.Open(dir)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("Failed to open BadgerDB store: %v", err)
	}

	// Return cleanup function
	cleanup := func() {
		store.Close()
		os.RemoveAll(dir)
	}

	return store, dir, cleanup
}

// TestBadgerStoreCRUD tests basic CRUD operations.
func TestBadgerStoreCRUD(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Test resource type
	resourceType := types.ResourceTypeService
	namespace := "default"
	name := "test-service"

	// Test resource
	type Service struct {
		Name  string `json:"name"`
		Image string `json:"image"`
		Port  int    `json:"port"`
	}

	service := Service{
		Name:  "test-service",
		Image: "nginx:latest",
		Port:  80,
	}

	// Test create
	err := store.Create(ctx, resourceType, namespace, name, service)
	if err != nil {
		t.Fatalf("Failed to create resource: %v", err)
	}

	// Test get
	var retrievedService Service
	err = store.Get(ctx, resourceType, namespace, name, &retrievedService)
	if err != nil {
		t.Fatalf("Failed to get resource: %v", err)
	}

	if retrievedService.Name != service.Name ||
		retrievedService.Image != service.Image ||
		retrievedService.Port != service.Port {
		t.Fatalf("Retrieved resource does not match original: %+v vs %+v", retrievedService, service)
	}

	// Test update
	updatedService := Service{
		Name:  "test-service",
		Image: "nginx:1.19",
		Port:  8080,
	}

	err = store.Update(ctx, resourceType, namespace, name, updatedService)
	if err != nil {
		t.Fatalf("Failed to update resource: %v", err)
	}

	// Verify the update
	var retrievedUpdatedService Service
	err = store.Get(ctx, resourceType, namespace, name, &retrievedUpdatedService)
	if err != nil {
		t.Fatalf("Failed to get updated resource: %v", err)
	}

	if retrievedUpdatedService.Name != updatedService.Name ||
		retrievedUpdatedService.Image != updatedService.Image ||
		retrievedUpdatedService.Port != updatedService.Port {
		t.Fatalf("Retrieved updated resource does not match updated: %+v vs %+v",
			retrievedUpdatedService, updatedService)
	}

	// Test list
	var resources []Service
	err = store.List(ctx, resourceType, namespace, &resources)
	if err != nil {
		t.Fatalf("Failed to list resources: %v", err)
	}

	if len(resources) != 1 {
		t.Fatalf("Expected 1 resource, got %d", len(resources))
	}

	// Test delete
	err = store.Delete(ctx, resourceType, namespace, name)
	if err != nil {
		t.Fatalf("Failed to delete resource: %v", err)
	}

	// Verify delete
	var deletedService Service
	err = store.Get(ctx, resourceType, namespace, name, &deletedService)
	if err == nil {
		t.Fatalf("Resource still exists after deletion")
	}
}

// TestBadgerStoreVersioning tests versioning functionality.
func TestBadgerStoreVersioning(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Test resource
	type Service struct {
		Name    string `json:"name"`
		Image   string `json:"image"`
		Version string `json:"version"`
	}

	resourceType := types.ResourceTypeService
	namespace := "default"
	name := "versioned-service"

	// Create initial version
	v1 := Service{
		Name:    "versioned-service",
		Image:   "app:v1",
		Version: "1.0.0",
	}

	err := store.Create(ctx, resourceType, namespace, name, v1)
	if err != nil {
		t.Fatalf("Failed to create initial version: %v", err)
	}

	// Small delay to ensure different timestamp for versions
	time.Sleep(10 * time.Millisecond)

	// Update to v2
	v2 := Service{
		Name:    "versioned-service",
		Image:   "app:v2",
		Version: "2.0.0",
	}

	err = store.Update(ctx, resourceType, namespace, name, v2)
	if err != nil {
		t.Fatalf("Failed to update to v2: %v", err)
	}

	// Small delay to ensure different timestamp for versions
	time.Sleep(10 * time.Millisecond)

	// Update to v3
	v3 := Service{
		Name:    "versioned-service",
		Image:   "app:v3",
		Version: "3.0.0",
	}

	err = store.Update(ctx, resourceType, namespace, name, v3)
	if err != nil {
		t.Fatalf("Failed to update to v3: %v", err)
	}

	// Get history
	history, err := store.GetHistory(ctx, resourceType, namespace, name)
	if err != nil {
		t.Fatalf("Failed to get history: %v", err)
	}

	// Should have 3 versions (most recent first because of reverse ordering)
	if len(history) != 3 {
		t.Fatalf("Expected 3 versions in history, got %d", len(history))
	}

	// Check that versions are in the correct order (newest first)
	versions := []string{"3.0.0", "2.0.0", "1.0.0"}
	for i, version := range history {
		service, ok := version.Resource.(map[string]interface{})
		if !ok {
			t.Fatalf("Failed to convert version %d to Service", i)
		}

		if service["version"] != versions[i] {
			t.Fatalf("Expected version %s at position %d, got %s",
				versions[i], i, service["version"])
		}
	}
}

// TestBadgerStoreTransaction tests transaction support.
func TestBadgerStoreTransaction(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Test service
	type Service struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}

	// Test successful transaction
	err := store.Transaction(ctx, func(tx Transaction) error {
		// Create multiple services in a transaction
		for i := 1; i <= 3; i++ {
			service := Service{
				Name:  fmt.Sprintf("service-%d", i),
				Image: fmt.Sprintf("image-%d:latest", i),
			}

			err := tx.Create(types.ResourceTypeService, "default", fmt.Sprintf("service-%d", i), service)
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}

	// Verify all services were created
	for i := 1; i <= 3; i++ {
		var service Service
		err := store.Get(ctx, types.ResourceTypeService, "default", fmt.Sprintf("service-%d", i), &service)
		if err != nil {
			t.Fatalf("Failed to get service-%d: %v", i, err)
		}

		expectedName := fmt.Sprintf("service-%d", i)
		expectedImage := fmt.Sprintf("image-%d:latest", i)

		if service.Name != expectedName || service.Image != expectedImage {
			t.Fatalf("Service-%d data incorrect, got %+v", i, service)
		}
	}

	// Test failed transaction (should rollback)
	err = store.Transaction(ctx, func(tx Transaction) error {
		// This will succeed
		service := Service{
			Name:  "will-rollback",
			Image: "rollback:latest",
		}

		err := tx.Create(types.ResourceTypeService, "default", "will-rollback", service)
		if err != nil {
			return err
		}

		// This will trigger a rollback, service already exists
		return tx.Create(types.ResourceTypeService, "default", "service-1", service)
	})

	if err == nil {
		t.Fatalf("Expected transaction to fail, but it succeeded")
	}

	// Verify the first service was not created due to rollback
	var rollbackService Service
	err = store.Get(ctx, types.ResourceTypeService, "default", "will-rollback", &rollbackService)
	if err == nil {
		t.Fatalf("Service 'will-rollback' should not exist due to transaction rollback")
	}
}

// TestBadgerStoreWatch tests the watch functionality.
func TestBadgerStoreWatch(t *testing.T) {
	store, _, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up watch for services in default namespace
	events, err := store.Watch(ctx, types.ResourceTypeService, "default")
	if err != nil {
		t.Fatalf("Failed to set up watch: %v", err)
	}

	// Test service
	type Service struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}

	service := Service{
		Name:  "watched-service",
		Image: "nginx:latest",
	}

	// Create service and capture event
	err = store.Create(ctx, types.ResourceTypeService, "default", "watched-service", service)
	if err != nil {
		t.Fatalf("Failed to create service: %v", err)
	}

	// Wait for the create event
	timeout := time.After(1 * time.Second)
	var createEvent WatchEvent

	select {
	case createEvent = <-events:
		// Got event
	case <-timeout:
		t.Fatalf("Timed out waiting for create event")
	}

	if createEvent.Type != WatchEventCreated {
		t.Fatalf("Expected create event, got %s", createEvent.Type)
	}

	if createEvent.ResourceType != types.ResourceTypeService ||
		createEvent.Namespace != "default" ||
		createEvent.Name != "watched-service" {
		t.Fatalf("Event contains incorrect data: %+v", createEvent)
	}

	// Update the service
	updatedService := Service{
		Name:  "watched-service",
		Image: "nginx:1.19",
	}

	err = store.Update(ctx, types.ResourceTypeService, "default", "watched-service", updatedService)
	if err != nil {
		t.Fatalf("Failed to update service: %v", err)
	}

	// Wait for the update event
	var updateEvent WatchEvent

	select {
	case updateEvent = <-events:
		// Got event
	case <-timeout:
		t.Fatalf("Timed out waiting for update event")
	}

	if updateEvent.Type != WatchEventUpdated {
		t.Fatalf("Expected update event, got %s", updateEvent.Type)
	}

	// Delete the service
	err = store.Delete(ctx, types.ResourceTypeService, "default", "watched-service")
	if err != nil {
		t.Fatalf("Failed to delete service: %v", err)
	}

	// Wait for the delete event
	var deleteEvent WatchEvent

	select {
	case deleteEvent = <-events:
		// Got event
	case <-timeout:
		t.Fatalf("Timed out waiting for delete event")
	}

	if deleteEvent.Type != WatchEventDeleted {
		t.Fatalf("Expected delete event, got %s", deleteEvent.Type)
	}
}
