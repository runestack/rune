//go:build !linux

package mountinfo

func Stack(string) ([]Dev, error) { return nil, ErrUnsupported }

func MountPoints(Dev) ([]string, error) { return nil, ErrUnsupported }

func MountedAt(string, string) (bool, error) { return false, ErrUnsupported }

func DeviceOf(string) (Dev, error) { return Dev{}, ErrUnsupported }
