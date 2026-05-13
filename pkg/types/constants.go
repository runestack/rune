package types

// Runtime types
type RuntimeType string

const (
	RuntimeTypeContainer RuntimeType = "container"
	RuntimeTypeProcess   RuntimeType = "process"
)

// ResourceType is the type of resource.
type ResourceType string

const (
	// ResourceTypeService is the resource type for services.
	ResourceTypeService ResourceType = "service"

	// ResourceTypeInstance is the resource type for instances.
	ResourceTypeInstance ResourceType = "instance"

	// ResourceTypeNamespace is the resource type for namespaces.
	ResourceTypeNamespace ResourceType = "namespace"

	// ResourceTypeScalingOperation is the resource type for scaling operations.
	ResourceTypeScalingOperation ResourceType = "scaling_operation"

	// ResourceTypeDeletionOperation is the resource type for deletion operations.
	ResourceTypeDeletionOperation ResourceType = "deletion_operation"

	// ResourceTypeSecret represents secrets (encrypted at rest)
	ResourceTypeSecret ResourceType = "secret"

	// ResourceTypeConfigmap represents non-sensitive configs
	ResourceTypeConfigmap ResourceType = "configmap"

	// ResourceTypeUser represents user identities
	ResourceTypeUser ResourceType = "user"

	// ResourceTypeToken represents authentication tokens
	ResourceTypeToken ResourceType = "token"

	// ResourceTypePolicy represents authorization policies
	ResourceTypePolicy ResourceType = "policy"

	// ResourceTypeAuditEvent represents append-only audit events.
	ResourceTypeAuditEvent ResourceType = "audit_event"

	// ResourceTypeVolume represents persistent volumes (RUNE-069).
	ResourceTypeVolume ResourceType = "volume"

	// ResourceTypeStorageClass represents cluster-scoped storage classes (RUNE-073).
	ResourceTypeStorageClass ResourceType = "storage_class"

	// ResourceTypeSnapshot represents volume snapshots (RUNE-071).
	ResourceTypeSnapshot ResourceType = "snapshot"
)

// RunnerType is the type of runner for an instance.
type RunnerType string

const (
	RunnerTypeTest    RunnerType = "test"
	RunnerTypeDocker  RunnerType = "docker"
	RunnerTypeProcess RunnerType = "process"
)
