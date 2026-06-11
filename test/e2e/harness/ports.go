//go:build e2e
// +build e2e

package harness

import (
	"fmt"
	"net"
	"testing"
)

// freeAddr asks the OS for a free 127.0.0.1 port and returns
// "127.0.0.1:<port>". The listener is closed before returning, so a
// tiny race with other processes exists, but binding loopback with
// OS-assigned ports makes collisions vanishingly rare in practice —
// and far rarer than the hardcoded ports this replaces.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("harness: allocate port: %v", err)
	}
	defer l.Close()
	return fmt.Sprintf("127.0.0.1:%d", l.Addr().(*net.TCPAddr).Port)
}
