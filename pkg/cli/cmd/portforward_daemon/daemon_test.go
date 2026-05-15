package portforwarddaemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/log"
)

func TestDaemonPingList_NoForwards(t *testing.T) {
	dir := shortTempDir(t)

	// Shorten idle window so the test doesn't hang waiting on the
	// 60s default.
	prev := IdleShutdownAfter
	IdleShutdownAfter = 200 * time.Millisecond
	defer func() { IdleShutdownAfter = prev }()

	d := New(dir, log.NewLogger(), func() (*client.Client, error) {
		// Not used in this test — we only exercise control-plane
		// commands (ping, list).
		return nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	sock := SocketPath(dir)
	if !waitForSocket(sock, 2*time.Second) {
		// Surface any startup error from Run rather than just
		// "socket missing" which hides the real cause.
		select {
		case err := <-done:
			t.Fatalf("daemon Run returned early: %v", err)
		default:
		}
		t.Fatalf("daemon socket never appeared")
	}

	// Ping.
	resp, err := Call(sock, Request{Cmd: CmdPing})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !resp.OK || resp.Version == "" {
		t.Fatalf("ping bad response: %+v", resp)
	}

	// List should be empty.
	resp, err = Call(sock, Request{Cmd: CmdList})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !resp.OK || len(resp.Forwards) != 0 {
		t.Fatalf("list bad response: %+v", resp)
	}

	// Unknown command path.
	resp, err = Call(sock, Request{Cmd: "nonsense"})
	if err != nil {
		t.Fatalf("nonsense call: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected error for unknown cmd, got %+v", resp)
	}

	// Idle watchdog should exit on its own.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exited with: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("daemon did not idle-exit")
	}
}

func TestStop_NoSuchForward(t *testing.T) {
	dir := shortTempDir(t)
	prev := IdleShutdownAfter
	IdleShutdownAfter = 200 * time.Millisecond
	defer func() { IdleShutdownAfter = prev }()

	d := New(dir, log.NewLogger(), func() (*client.Client, error) { return nil, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	sock := SocketPath(dir)
	if !waitForSocket(sock, 2*time.Second) {
		t.Fatal("socket never appeared")
	}

	resp, err := Call(sock, Request{Cmd: CmdStop, ID: "fwd-missing"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Fatalf("expected error for missing forward, got %+v", resp)
	}

	cancel()
	<-done
}

// shortTempDir creates a per-test directory under /tmp to keep unix
// socket paths under macOS's 104-byte sun_path limit. The default
// t.TempDir() lives in /var/folders/... which blows past the limit
// when combined with a long test name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rpf-")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func waitForSocket(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isReachable(path) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
