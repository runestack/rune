// Driver-capability lint for Volume resources at the API write path.
//
// Today the storage drivers enforce capability invariants at provision
// time (Driver.Provision returns an error and the controller marks the
// Volume Failed/Stalled). Surfacing the same checks at the gRPC write
// path turns "operator submits cast → orchestrator quietly fails on
// next reconcile" into "operator submits cast → cast fails with a
// clear error". This file implements the cast-time half of that
// guarantee: AccessMode must be in driver Capabilities; local-host
// volumes must declare a parameters.hostPath that resolves inside the
// runefile allowlist. Anything we cannot determine without driver
// instantiation (e.g. block-device formatting) is intentionally left
// to the controller.
//
// All checks fail OPEN: if the StorageClass is missing, if the driver
// is unregistered, or if driver instantiation errors, the lint defers
// to the controller. This keeps the unit-test surface small (callers
// that did not seed a StorageClass keep working) while the production
// path — where the controller seeds the built-in classes at boot — is
// fully covered.
package service

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/storage/driver/local"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// validateAgainstStorageClass runs cast-time driver-capability checks
// against the volume's referenced StorageClass. Returns a user-facing
// error suitable for codes.InvalidArgument.
//
// Behaviour:
//   - StorageClass missing                  → defer (no error; controller will surface)
//   - Driver not registered                 → defer
//   - Driver factory error / no driver      → defer
//   - AccessMode not in Capabilities        → reject with the supported set
//   - local-host: hostPath missing/relative → reject
//   - local-host: hostPath outside allowlist→ reject
func (s *VolumeService) validateAgainstStorageClass(ctx context.Context, v *types.Volume) error {
	if v == nil || v.StorageClassName == "" {
		return nil
	}
	sc, err := s.scRepo.Get(ctx, v.StorageClassName)
	if err != nil {
		if store.IsNotFoundError(err) {
			s.logger.Debug("volume lint: storage class not found, deferring",
				log.Str("storageClass", v.StorageClassName),
				log.Str("volume", v.Namespace+"/"+v.Name))
			return nil
		}
		return fmt.Errorf("lookup storage class %q: %w", v.StorageClassName, err)
	}
	caps, ok := s.lookupCapabilities(sc.Driver)
	if !ok {
		s.logger.Debug("volume lint: driver not available, deferring",
			log.Str("driver", sc.Driver),
			log.Str("volume", v.Namespace+"/"+v.Name))
		return nil
	}
	if v.AccessMode != "" {
		if !accessModeSupported(v.AccessMode, caps.AccessModes) {
			return fmt.Errorf("accessMode %q not supported by storage class %q (driver %q supports %v)",
				v.AccessMode, sc.Name, sc.Driver, caps.AccessModes)
		}
	}
	if sc.Driver == local.DriverNameLocalHost {
		if err := s.validateLocalHostHostPath(sc, v); err != nil {
			return err
		}
	}
	return nil
}

// lookupCapabilities instantiates the named driver with its runefile
// config and returns the Capabilities. Returns ok=false if the driver
// is not registered or the factory errors — callers should defer to
// the controller in that case.
func (s *VolumeService) lookupCapabilities(name string) (driver.Capabilities, bool) {
	factory, ok := driver.Lookup(name)
	if !ok {
		return driver.Capabilities{}, false
	}
	d, err := factory(s.driverConfigs[name])
	if err != nil {
		return driver.Capabilities{}, false
	}
	return d.Capabilities(), true
}

// validateLocalHostHostPath enforces that a local-host volume names an
// absolute hostPath inside the runefile's hostPathAllowlist. Mirrors
// the check done by hostDriver.Provision so operators see the error at
// cast time instead of waiting for the volume to flip to Failed.
func (s *VolumeService) validateLocalHostHostPath(sc *types.StorageClass, v *types.Volume) error {
	merged := mergeStringMap(sc.Parameters, v.Parameters)
	hostPath := merged["hostPath"]
	if hostPath == "" {
		return fmt.Errorf("storage class %q (driver local-host) requires parameters.hostPath", sc.Name)
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return fmt.Errorf("parameters.hostPath %q is not a valid path: %v", hostPath, err)
	}
	allowlist := local.AllowlistFromConfig(s.driverConfigs[local.DriverNameLocalHost])
	if !local.IsHostPathAllowed(abs, allowlist) {
		return fmt.Errorf("parameters.hostPath %q is not in the runefile hostPathAllowlist (%v)", abs, allowlist)
	}
	return nil
}

// accessModeSupported reports whether mode is in the driver's
// supported set.
func accessModeSupported(mode types.AccessMode, supported []types.AccessMode) bool {
	for _, m := range supported {
		if m == mode {
			return true
		}
	}
	return false
}

// mergeStringMap returns a new map equivalent to merging overrides on
// top of base (overrides wins per-key). Either input may be nil.
func mergeStringMap(base, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
