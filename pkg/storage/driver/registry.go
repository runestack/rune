// Package driver — registry for storage drivers.
//
// Drivers register themselves in init() via Register(). The runed binary
// blank-imports each built-in driver package from cmd/runed/main.go;
// out-of-tree drivers are added by adding one more blank-import.
package driver

import (
	"fmt"
	"sort"
	"sync"
)

// Factory constructs a Driver from operator-supplied config (parsed from
// the runefile [storage.<name>] section). Returning an error here is fatal —
// the controller will refuse to start with an unconfigurable driver.
//
// config is the raw map for the runefile section; drivers decode it
// themselves to keep this package free of viper/koanf dependencies.
type Factory func(config map[string]any) (Driver, error)

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Factory)
)

// Register adds a driver factory under the given name. Panics if name is
// empty or already registered — driver registration happens at init() time
// in operator-controlled code, so a duplicate name is a programming bug
// that must not be papered over at runtime.
func Register(name string, factory Factory) {
	if name == "" {
		panic("storage driver: Register called with empty name")
	}
	if factory == nil {
		panic("storage driver: Register called with nil factory for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("storage driver: " + name + " is already registered")
	}
	registry[name] = factory
}

// Lookup returns the factory registered under name, or false if none.
// Used by the controller at boot to instantiate drivers referenced by
// StorageClass.Driver.
func Lookup(name string) (Factory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}

// MustLookup is the panic-on-missing variant. Reserved for tests and
// init-time wiring where a missing driver is unambiguously a bug.
func MustLookup(name string) Factory {
	f, ok := Lookup(name)
	if !ok {
		panic("storage driver: no driver registered under " + name)
	}
	return f
}

// New is a convenience that looks up name and invokes the factory with
// config. Returns a wrapped error if either step fails.
func New(name string, config map[string]any) (Driver, error) {
	factory, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("storage driver: %w: %q", ErrNotFound, name)
	}
	d, err := factory(config)
	if err != nil {
		return nil, fmt.Errorf("storage driver %q: %w", name, err)
	}
	if d.Name() != name {
		return nil, fmt.Errorf("storage driver %q: factory returned driver named %q", name, d.Name())
	}
	return d, nil
}

// Registered returns the sorted list of currently-registered driver names.
// Useful for diagnostics and the API-server's StorageClass linter.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resetForTest clears the registry. Test-only — never call from production
// code. Exported via registry_testing.go for use by test packages.
func resetForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = make(map[string]Factory)
}
