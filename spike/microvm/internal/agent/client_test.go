package agent

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/spike/microvm/internal/proto"
)

// fakeMuxer imitates Firecracker's host-side vsock socket: it expects
// "CONNECT <port>\n", and drops the first `refuse` connections the way
// Firecracker does while nothing listens on the guest port.
func fakeMuxer(t *testing.T, refuse int32) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "v.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var seen atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				line, err := r.ReadString('\n')
				if err != nil || line != "CONNECT 1024\n" {
					return
				}
				if seen.Add(1) <= refuse {
					return
				}
				conn.Write([]byte("OK 1073741824\n"))
				for {
					var req proto.Request
					if err := proto.Read(r, &req); err != nil {
						return
					}
					resp := proto.Response{ID: req.ID, OK: true}
					if req.Op == proto.OpExec {
						resp.Exec = &proto.ExecResult{Stdout: []byte(req.Exec.Argv[0])}
					}
					if err := proto.Write(conn, &resp); err != nil {
						return
					}
				}
			}()
		}
	}()
	return sock
}

func TestWaitDialRetriesUntilAgentListens(t *testing.T) {
	sock := fakeMuxer(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := WaitDial(ctx, sock, proto.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.Exec(ctx, "true")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "true" {
		t.Fatalf("stdout %q", res.Stdout)
	}
}

func TestWaitDialTimesOut(t *testing.T) {
	sock := fakeMuxer(t, 1<<30)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := WaitDial(ctx, sock, proto.AgentPort); err == nil {
		t.Fatal("expected a timeout")
	}
}
