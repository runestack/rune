package types

import (
	"time"
)

// Secret represents a securely stored piece of sensitive data.
type Secret struct {
	// Unique identifier for the secret
	ID string `json:"id" yaml:"id"`

	// Human-readable name for the secret (DNS-1123 unique name within a namespace)
	Name string `json:"name" yaml:"name"`

	// Namespace the secret belongs to
	Namespace string `json:"namespace" yaml:"namespace"`

	// Type of secret (static or dynamic)
	Type string `json:"type" yaml:"type"`

	// Static data (encrypted at rest, only for static secrets)
	Data map[string]string `json:"-" yaml:"-"` // Not serialized, stored encrypted separately

	// DataKeys is the sorted list of keys present in Data. It is populated
	// by metadata-only API responses (Get, List) so callers can see the
	// shape of a secret without learning its values. Reveal responses
	// populate both Data and DataKeys.
	DataKeys []string `json:"dataKeys,omitempty" yaml:"dataKeys,omitempty"`

	// Engine configuration for dynamic secrets
	Engine *SecretEngine `json:"engine,omitempty" yaml:"engine,omitempty"`

	// Rotation configuration
	Rotation *RotationPolicy `json:"rotation,omitempty" yaml:"rotation,omitempty"`

	// Current version number
	Version int `json:"version" yaml:"version"`

	// Creation timestamp
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`

	// Last rotation timestamp
	LastRotated *time.Time `json:"lastRotated,omitempty" yaml:"lastRotated,omitempty"`
}

// StoredSecret is the persisted encrypted form of a Secret
type StoredSecret struct {
	// Name of the secret
	Name string `json:"name"`

	// Namespace of the secret
	Namespace string `json:"namespace"`

	// Version of the secret
	Version int `json:"version"`

	// Creation timestamp
	CreatedAt time.Time `json:"createdAt"`

	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt"`

	// Ciphertext of the secret
	Ciphertext []byte `json:"ciphertext"`

	// Wrapped DEK of the secret
	WrappedDEK []byte `json:"wrappedDEK"`
}

// SecretEngine defines a dynamic secret engine configuration.
type SecretEngine struct {
	// Name of the engine to use (references a SecretsEngine resource)
	Name string `json:"name" yaml:"name"`

	// Role or profile to use with the engine (engine-specific)
	Role string `json:"role,omitempty" yaml:"role,omitempty"`

	// Engine-specific configuration for this secret
	Config map[string]interface{} `json:"config,omitempty" yaml:"config,omitempty"`
}

// RotationPolicy defines when and how to rotate a secret.
type RotationPolicy struct {
	// Interval between rotations (e.g., "30d", "90d")
	Interval string `json:"interval" yaml:"interval"`

	// Actions to take after rotation
	OnRotate []RotationAction `json:"onRotate,omitempty" yaml:"onRotate,omitempty"`
}

// RotationAction defines an action to take after secret rotation.
type RotationAction struct {
	// Services to reload (send SIGHUP)
	ReloadServices []string `json:"reloadServices,omitempty" yaml:"reloadServices,omitempty"`

	// Services to restart (rolling restart)
	RestartServices []string `json:"restartServices,omitempty" yaml:"restartServices,omitempty"`

	// Job to run
	RunJob string `json:"runJob,omitempty" yaml:"runJob,omitempty"`
}

// SecretsEngine represents a configured secrets engine.
type SecretsEngine struct {
	// Unique identifier for the engine
	ID string `json:"id" yaml:"id"`

	// Human-readable name for the engine
	Name string `json:"name" yaml:"name"`

	// Namespace the engine belongs to
	Namespace string `json:"namespace" yaml:"namespace"`

	// Type of engine (builtin, function, plugin)
	Type string `json:"type" yaml:"type"`

	// For function-based engines, the reference to the function
	Function string `json:"function,omitempty" yaml:"function,omitempty"`

	// For plugin-based engines, the path to the plugin executable
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// Engine-wide configuration
	Config map[string]interface{} `json:"config,omitempty" yaml:"config,omitempty"`

	// Creation timestamp
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}
