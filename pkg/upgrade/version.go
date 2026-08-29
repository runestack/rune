package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ParseVersion parses a release tag or stamped version ("v0.0.1-dev.150").
// Prerelease identifiers compare numerically under semver, so
// dev.150 > dev.16 — a naive string compare gets that wrong, which is why
// this package never compares version strings directly. A from-source build
// stamps "dev" (pkg/version), which does not parse; callers must handle
// that by requiring an explicit target rather than guessing.
func ParseVersion(s string) (*semver.Version, error) {
	v, err := semver.NewVersion(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("unparseable version %q: %w", s, err)
	}
	return v, nil
}

// CompareVersions returns -1/0/1 for a<b, a==b, a>b.
func CompareVersions(a, b string) (int, error) {
	av, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	bv, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	return av.Compare(bv), nil
}

// FloorPath is the root-owned monotonic version floor. The applier refuses
// any target below it, which is what stops a MITM'd request or a
// compromised service account from forcing a downgrade to a
// known-vulnerable official build; lowering it is a deliberate root action.
// Installers seed it at install time — without seeding, a fresh host would
// be downgrade-forceable until its first in-band upgrade.
const FloorPath = "/etc/rune/version-floor"

// ErrFloorUnparseable marks a floor file that exists but does not parse.
// This fails the apply closed: corruption must not silently disable
// downgrade protection. (Absence, by contrast, is the pre-seeding state
// and allows with a loud log line.)
type ErrFloorUnparseable struct{ Raw string }

func (e *ErrFloorUnparseable) Error() string {
	return fmt.Sprintf("version floor %s is unparseable (%q); refusing to apply — fix or remove the file (root)", FloorPath, e.Raw)
}

// ReadFloor returns the floor version, or (nil, nil) when the file is
// absent, or *ErrFloorUnparseable when present but invalid.
func ReadFloor(path string) (*semver.Version, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(b))
	v, err := semver.NewVersion(raw)
	if err != nil {
		return nil, &ErrFloorUnparseable{Raw: raw}
	}
	return v, nil
}

// WriteFloor atomically writes the floor. It refuses to write a value it
// cannot parse back.
func WriteFloor(path, version string) error {
	if _, err := ParseVersion(version); err != nil {
		return fmt.Errorf("refusing to write floor: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".version-floor-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.TrimSpace(version) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
