//go:build !linux

package main

import (
	"fmt"
	"io"
)

// The applier drives systemd and swaps linux binaries; on any other
// platform the subcommand exists only to fail with a clear message
// instead of falling through to the daemon.
func runApplyUpgrade(_ []string, _, stderr io.Writer) int {
	fmt.Fprintln(stderr, "runed apply-upgrade: only supported on linux systemd hosts")
	return 1
}
