//go:build !linux

package mountsync

// The cloud volume drivers only mount on Linux nodes. These exist so the
// packages that call them still build for local development on macOS.

func syncTarget(string) error { return nil }

func unmountTarget(string, string) error { return nil }
