//go:build !linux

package vip

import "net"

// checkRouteCollision is a no-op on non-Linux platforms. macOS dev
// hosts don't expose host routes via a portable Go API; production
// runs on Linux where the real check applies.
func checkRouteCollision(_ *net.IPNet) error { return nil }
