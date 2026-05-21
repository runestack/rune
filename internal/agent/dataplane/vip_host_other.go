//go:build !linux

package dataplane

import "net"

// localVIPHost is a no-op off Linux (dev-mode uses 127.0.0.1 listeners).
type localVIPHost struct{}

func newLocalVIPHost() *localVIPHost { return &localVIPHost{} }

func (h *localVIPHost) add(net.IP) error { return nil }
func (h *localVIPHost) remove(net.IP)    {}
