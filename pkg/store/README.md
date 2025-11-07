# Rune State Store

The state store package provides persistent storage for Rune's resources, supporting CRUD operations, versioning, watching for changes, and transactional updates.

## Overview

The store package implements a flexible, extensible storage interface with BadgerDB as the primary implementation. The store provides:

- CRUD operations for any resource type
- Namespaced resources
- Resource versioning and history
- Atomic transactions
- Watch mechanisms for reactive patterns

## Interface

The core `Store` interface provides the following operations:

```go
// Store defines the interface for state storage
type Store interface {
    Open(path string) error
    Close() error
    Create(ctx context.Context, resourceType string, namespace string, name string, resource interface{}) error
    Get(ctx context.Context, resourceType string, namespace string, name string, resource interface{}) error
    List(ctx context.Context, resourceType string, namespace string) ([]interface{}, error)
    Update(ctx context.Context, resourceType string, namespace string, name string, resource interface{}) error
    Delete(ctx context.Context, resourceType string, namespace string, name string) error
    Watch(ctx context.Context, resourceType string, namespace string) (<-chan WatchEvent, error)
    Transaction(ctx context.Context, fn func(tx Transaction) error) error
    GetHistory(ctx context.Context, resourceType string, namespace string, name string) ([]HistoricalVersion, error)
    GetVersion(ctx context.Context, resourceType string, namespace string, name string, version string) (interface{}, error)
}
```

## Repositories (Adapters)

To provide a clean, testable API per resource without duplicating storage logic, we introduce small, typed repository adapters over the single core `Store`. These repos accept a standard reference (ref) string for addressing resources and delegate to the core store using consistent keys and versioning from `util.go`.

- One DB (Badger), one `Store` implementation
- Repos live under `pkg/store/repos/`
- Secrets are encrypted at rest via a codec within the core store (transparent to callers)

### Resource Reference (Ref)

Canonical URI form:

- Name-based: `rune://<type>/<namespace>/<name>`
- ID-based:   `rune://<type>/<namespace>/id/<id>`

Shorthand FQDNs like `secret:db-credentials.prod.rune` are also supported by the parser.

Helpers:

- `ParseRef(string) (Ref, error)`
- `FormatRef(rt, ns, name string) string`

### Example (SecretRepo)

```go
core := store.NewBadgerStore(logger)
_ = core.Open("/path/to/data")
defer core.Close()

secRepo := repos.NewSecretRepo(core)

ref := repos.FormatRef(types.ResourceTypeSecret, "prod", "db-credentials")
secret := &types.Secret{Name: "db-credentials", Namespace: "prod", Type: "static", Data: map[string]string{"username":"admin","password":"s3cr3t"}}

if err := secRepo.Create(ctx, ref, secret); err != nil { /* handle */ }
got, err := secRepo.Get(ctx, ref)
```

### Base Repo

`BaseRepo[T]` provides common `Create/Get/Update/Delete` on top of the core `Store`. Resource-specific repos compose it and add behavior only as needed. Secrets use transparent encryption at rest; configs use default JSON.

## Usage

### Creating a Store

```go
import (
    "context"
    "log"
    "razorbill.dev/rune/pkg/store"
)

// Create a BadgerDB store
logger := log.New(os.Stdout, "", log.LstdFlags)
s := store.NewBadgerStore(logger)

// Open the store
if err := s.Open("/path/to/data"); err != nil {
    log.Fatalf("Failed to open store: %v", err)
}
defer s.Close()
```

### Working with Resources

```go
// Define a resource
type Service struct {
    Name  string   `json:"name"`
    Image string   `json:"image"`
    Ports []int    `json:"ports"`
}

// Create a resource
service := Service{
    Name:  "my-service",
    Image: "nginx:latest",
    Ports: []int{80, 443},
}

ctx := context.Background()
if err := s.Create(ctx, "services", "default", "my-service", service); err != nil {
    log.Fatalf("Failed to create service: %v", err)
}

// Get a resource
var retrievedService Service
if err := s.Get(ctx, "services", "default", "my-service", &retrievedService); err != nil {
    log.Fatalf("Failed to get service: %v", err)
}

// Update a resource
service.Image = "nginx:1.19"
if err := s.Update(ctx, "services", "default", "my-service", service); err != nil {
    log.Fatalf("Failed to update service: %v", err)
}

// List resources
services, err := s.List(ctx, "services", "default")
if err != nil {
    log.Fatalf("Failed to list services: %v", err)
}

// Delete a resource
if err := s.Delete(ctx, "services", "default", "my-service"); err != nil {
    log.Fatalf("Failed to delete service: %v", err)
}
```

### Transactions

```go
// Execute multiple operations in a transaction
err := s.Transaction(ctx, func(tx store.Transaction) error {
    // Create a service
    service := Service{Name: "service-1", Image: "image-1"}
    if err := tx.Create("services", "default", "service-1", service); err != nil {
        return err
    }
    
    // Create another service
    service2 := Service{Name: "service-2", Image: "image-2"}
    if err := tx.Create("services", "default", "service-2", service2); err != nil {
        return err
    }
    
    return nil
})
```

### Watching for Changes

```go
// Watch for changes to services in the default namespace
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

events, err := s.Watch(ctx, "services", "default")
if err != nil {
    log.Fatalf("Failed to set up watch: %v", err)
}

go func() {
    for event := range events {
        switch event.Type {
        case store.WatchEventCreated:
            log.Printf("Service created: %s", event.Name)
        case store.WatchEventUpdated:
            log.Printf("Service updated: %s", event.Name)
        case store.WatchEventDeleted:
            log.Printf("Service deleted: %s", event.Name)
        }
    }
}()
```

### Version History

```go
// Get version history
history, err := s.GetHistory(ctx, "services", "default", "my-service")
if err != nil {
    log.Fatalf("Failed to get history: %v", err)
}

// Get a specific version
version := history[1].Version
oldVersion, err := s.GetVersion(ctx, "services", "default", "my-service", version)
if err != nil {
    log.Fatalf("Failed to get version: %v", err)
}
```

## Implementation Notes

- Resources are stored as serialized JSON (secrets are encrypted at rest transparently)
- Keys are organized hierarchically by resource type, namespace, and name
- Versions are stored separately with metadata and timestamps
- Watch events are distributed through an in-memory pub/sub system

### Resource Types

The core recognizes standard types, including:

- `service`, `instance`, `namespace`, `scaling_operation`, `deletion_operation`
- `secret` (encrypted at rest), `config`

## Future Enhancements

- Addition of a metrics subsystem for monitoring store operations
- Advanced query capabilities with filtering and sorting
- Pluggable serialization formats
- Integration with external storage systems (etcd, etc.)

## Testing

### TestStore

The package includes a `TestStore` implementation specifically designed for testing:

```go
import (
    "context"
    "testing"
    "github.com/runestack/rune/pkg/store"
    "github.com/runestack/rune/pkg/types"
)

func TestMyFunction(t *testing.T) {
    // Create a new TestStore
    testStore := store.NewTestStore()
    
    // Use the convenience methods to add test data
    ctx := context.Background()
    
    // Create test services and instances
    service := &types.Service{
        ID:        "test-service",
        Name:      "test-service",
        Namespace: "default",
    }
    
    instance := &types.Instance{
        ID:        "test-instance",
        Name:      "test-instance",
        Namespace: "default",
        ServiceID: "test-service",
    }
    
    // Add to store with helper methods
    testStore.CreateService(ctx, service)
    testStore.CreateInstance(ctx, instance)
    
    // Function to test
    result := MyFunctionThatUsesStore(testStore)
    
    // Assertions
    // ...
}
```

### Advantages of TestStore

The `TestStore` offers several advantages for testing:

1. **No external dependencies**: Works in-memory without need for file system access
2. **Built-in helper methods**: Simplified creation and retrieval of common types
3. **Type-aware**: Handles type copying and conversion automatically
4. **Reset capability**: Easily clear all data between tests
5. **Simplified setup**: Directly load test data with `SetupTestData` method

### Mock vs TestStore

While the package also includes a `MockStore` based on testify/mock, `TestStore` is often more convenient:

- Use `MockStore` when you need to verify specific method calls or simulate errors
- Use `TestStore` when you need a working store implementation with real data

The `TestStore` implementation is more natural for integration testing, while `MockStore` is better for strict unit testing with precise expectations. 