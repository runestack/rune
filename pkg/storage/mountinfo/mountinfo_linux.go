//go:build linux

package mountinfo

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Stack returns the devices mounted exactly at path, bottom first. An
// empty result means nothing is mounted there.
func Stack(path string) ([]Dev, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("mountinfo: %w", err)
	}
	defer func() { _ = f.Close() }()
	return parseStack(f, path)
}

// MountPoints returns every path dev is mounted at in this mount
// namespace.
func MountPoints(dev Dev) ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("mountinfo: %w", err)
	}
	defer func() { _ = f.Close() }()
	return parseMountPoints(f, dev)
}

// MountedAt reports whether dev is the visible mount at target. It errs
// when another device is mounted there, or when either cannot be read:
// a probe that cannot answer must not read as "not mounted", since the
// caller would then mount over whatever is there.
func MountedAt(target, dev string) (bool, error) {
	own, err := DeviceOf(dev)
	if err != nil {
		return false, err
	}
	stack, err := Stack(target)
	if err != nil {
		return false, err
	}
	return topIs(stack, own, target)
}

// DeviceOf returns the major:minor of a block device node, following
// symlinks such as /dev/disk/by-id/*.
func DeviceOf(path string) (Dev, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return Dev{}, fmt.Errorf("mountinfo: stat %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return Dev{}, fmt.Errorf("mountinfo: %s is not a block device", path)
	}
	return Dev{Major: unix.Major(uint64(st.Rdev)), Minor: unix.Minor(uint64(st.Rdev))}, nil //nolint:unconvert // Rdev is not uint64 on every arch
}
