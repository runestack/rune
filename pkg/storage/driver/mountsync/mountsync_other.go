//go:build !linux

package mountsync

import "errors"

// mountsync.go is untagged, so this keeps the package compiling on
// darwin. Nothing outside this package calls it — each driver has its own
// !linux mount path — and it returns an error rather than nil so that
// whoever does wire it up finds out immediately, instead of getting a
// silent success on a data-loss-critical routine.
func unmountTarget(string, string) error {
	return errors.New("mountsync: unmount is not supported on this platform")
}
