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

// Limits defines per-resource limits
type Limits struct {
	MaxObjectBytes   int
	MaxKeyNameLength int
}
