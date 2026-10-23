//go:build !linux

package mountinfo

import "errors"

// ErrUnsupported is returned off Linux, where no cloud volume driver
// mounts anything.
var ErrUnsupported = errors.New("mountinfo: not supported on this platform")

func Stack(string) ([]Dev, error) { return nil, ErrUnsupported }

func DeviceOf(string) (Dev, error) { return Dev{}, ErrUnsupported }
