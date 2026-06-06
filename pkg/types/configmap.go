package types

import "time"

// Configmap represents a piece of non-sensitive configuration data.
type Configmap struct {
	// Unique identifier for the config
	ID string `json:"id" yaml:"id"`

	// Human-readable name for the config (DNS-1123 unique name within a namespace)
	Name string `json:"name" yaml:"name"`

	// Namespace the config belongs to
	Namespace string `json:"namespace" yaml:"namespace"`

	// Configuration data (not encrypted)
	Data map[string]string `json:"data" yaml:"data"`

	// Current version number
	Version int `json:"version" yaml:"version"`

	// Creation timestamp
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`

	// OwnedBy is the system ownership stamp set when a runeset release manages
	// this configmap. See _docs/plugins/RUNESET_STATEFUL_RELEASES.md.
	OwnedBy *OwnedBy `json:"ownedBy,omitempty" yaml:"ownedBy,omitempty"`
}
