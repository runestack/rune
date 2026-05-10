package types

import "time"

// AuditOutcome captures whether the audited operation succeeded or was denied.
type AuditOutcome string

const (
	AuditOutcomeSuccess AuditOutcome = "success"
	AuditOutcomeDenied  AuditOutcome = "denied"
	AuditOutcomeError   AuditOutcome = "error"
)

// AuditEvent records a single security-relevant operation. It is append-only
// and intended to provide a queryable trail separate from the structured
// application log. Stored as a regular store resource under the system namespace.
type AuditEvent struct {
	// ID is a unique identifier for the event; ulid-or-uuid string.
	ID string `json:"id" yaml:"id"`

	// Timestamp is when the event was emitted (server clock).
	Timestamp time.Time `json:"timestamp" yaml:"timestamp"`

	// Actor is the authenticated subject ID, or "anonymous" if no auth was
	// established (e.g., bootstrap path) or "unknown" if we could not extract
	// a subject from the request context.
	Actor string `json:"actor" yaml:"actor"`

	// Action is the verb performed (create, update, delete, get, reveal, ...).
	Action string `json:"action" yaml:"action"`

	// Resource is the resource type (e.g., "secrets", "configmaps").
	Resource string `json:"resource" yaml:"resource"`

	// ResourceRef is the canonical "<namespace>/<name>" reference of the
	// affected object, when applicable. Empty for namespace-less actions.
	ResourceRef string `json:"resourceRef,omitempty" yaml:"resourceRef,omitempty"`

	// Namespace is the namespace of the resource (denormalized from ResourceRef
	// for cheap filtering).
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Outcome reports whether the operation succeeded.
	Outcome AuditOutcome `json:"outcome" yaml:"outcome"`

	// Message is an optional human-readable note (failure reason, etc.).
	// MUST NOT contain plaintext secret payloads.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	// Metadata is a free-form bag for non-sensitive context (e.g., key set
	// hash, version number, previous version on rollback).
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}
