package store

import (
	"github.com/runestack/rune/pkg/crypto"
)

// StoreOptions configure the core store
type StoreOptions struct {
	// Path is the directory path for Badger (if separate from Open arg)
	Path string

	// SecretEncryptionEnabled toggles secret at-rest encryption availability
	SecretEncryptionEnabled bool

	// KEKOptions configures loading the master key for wrapping secret DEKs
	KEKOptions *crypto.KEKOptions

	// KEKBytes is a precomputed KEK for tests
	KEKBytes []byte

	// Limits for secrets and configs
	SecretLimits Limits
	ConfigLimits Limits
}

// Limits defines per-resource limits. The yaml tags are the runefile keys
// under [secret.limits] and [config.limits]; without them the decoder only
// matched the run-together field names, so the documented keys were ignored.
type Limits struct {
	MaxObjectBytes   int `yaml:"max_object_bytes"`
	MaxKeyNameLength int `yaml:"max_key_name_length"`
}
