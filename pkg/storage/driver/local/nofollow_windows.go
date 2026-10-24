//go:build windows

package local

import "os"

// No volume driver runs on Windows; this keeps the package building.
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
