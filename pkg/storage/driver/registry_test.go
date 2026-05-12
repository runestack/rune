package driver_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

// stubDriver is a minimal Driver used to verify registry plumbing without
// pulling in the local driver (which has its own init()).
type stubDriver struct{ name string }

func (s *stubDriver) Name() string { return s.name }
func (s *stubDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{AccessModes: []types.AccessMode{types.AccessModeRWO}}
}
func (s *stubDriver) Provision(context.Context, driver.ProvisionRequest) (driver.VolumeHandle, error) {
	return "", nil
}
func (s *stubDriver) Delete(context.Context, driver.VolumeHandle) error { return nil }
func (s *stubDriver) Attach(context.Context, driver.VolumeHandle, driver.NodeID) (driver.DevicePath, error) {
	return "", nil
}
func (s *stubDriver) Detach(context.Context, driver.VolumeHandle, driver.NodeID) error { return nil }
func (s *stubDriver) Mount(context.Context, driver.MountOpts) (driver.MountTarget, error) {
	return "", nil
}
func (s *stubDriver) Unmount(context.Context, driver.MountTarget) error { return nil }
func (s *stubDriver) Snapshot(context.Context, driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	return "", driver.ErrUnsupported
}
func (s *stubDriver) RestoreFromSnapshot(context.Context, driver.RestoreRequest) (driver.VolumeHandle, error) {
	return "", driver.ErrUnsupported
}
func (s *stubDriver) DeleteSnapshot(context.Context, driver.SnapshotHandle) error {
	return driver.ErrUnsupported
}
func (s *stubDriver) Expand(context.Context, driver.VolumeHandle, string) error {
	return driver.ErrUnsupported
}

// uniqueRegister picks a name unique-enough that parallel test runs don't
// collide with other registrants in the same process.
var registerCounter struct {
	sync.Mutex
	n int
}

func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	registerCounter.Lock()
	defer registerCounter.Unlock()
	registerCounter.n++
	return prefix + "-" + t.Name() + "-" + sprintfInt(registerCounter.n)
}

func sprintfInt(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestRegisterAndLookup(t *testing.T) {
	name := uniqueName(t, "stub")
	driver.Register(name, func(map[string]any) (driver.Driver, error) {
		return &stubDriver{name: name}, nil
	})

	factory, ok := driver.Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q): not found", name)
	}
	d, err := factory(nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if d.Name() != name {
		t.Fatalf("Name = %q, want %q", d.Name(), name)
	}
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	name := uniqueName(t, "dup")
	driver.Register(name, func(map[string]any) (driver.Driver, error) {
		return &stubDriver{name: name}, nil
	})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	driver.Register(name, func(map[string]any) (driver.Driver, error) {
		return &stubDriver{name: name}, nil
	})
}

func TestRegisterPanicsOnEmptyName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty name")
		}
	}()
	driver.Register("", func(map[string]any) (driver.Driver, error) { return nil, nil })
}

func TestRegisterPanicsOnNilFactory(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil factory")
		}
	}()
	driver.Register(uniqueName(t, "nil"), nil)
}

func TestNewWrapsErrNotFound(t *testing.T) {
	_, err := driver.New(uniqueName(t, "nope"), nil)
	if !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("New(unknown): want ErrNotFound, got %v", err)
	}
}

func TestNewRejectsMisnamedFactory(t *testing.T) {
	name := uniqueName(t, "wrong")
	driver.Register(name, func(map[string]any) (driver.Driver, error) {
		return &stubDriver{name: "different-name"}, nil
	})
	_, err := driver.New(name, nil)
	if err == nil {
		t.Fatal("expected error from misnamed factory")
	}
}

func TestRegisteredIsSorted(t *testing.T) {
	// Register a couple in non-alphabetical order then ensure the listing
	// is sorted. We can't drop the registry between tests, so we just check
	// that our two names appear in sorted relative order.
	a := uniqueName(t, "zzz-aaa")
	b := uniqueName(t, "zzz-bbb")
	driver.Register(a, func(map[string]any) (driver.Driver, error) { return &stubDriver{name: a}, nil })
	driver.Register(b, func(map[string]any) (driver.Driver, error) { return &stubDriver{name: b}, nil })

	names := driver.Registered()
	var foundA, foundB int = -1, -1
	for i, n := range names {
		if n == a {
			foundA = i
		}
		if n == b {
			foundB = i
		}
	}
	if foundA == -1 || foundB == -1 {
		t.Fatalf("Registered missing test entries: %v", names)
	}
	if foundA >= foundB {
		t.Fatalf("Registered not sorted: %s appeared after %s", a, b)
	}
}
