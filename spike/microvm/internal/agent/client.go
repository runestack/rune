// Package agent is the host side of the guest agent protocol: it reaches the
// agent through Firecracker's vsock unix socket ("CONNECT <port>\n", then
// "OK <port>\n") and exchanges proto frames.
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/spike/microvm/internal/proto"
)

type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	next uint64
}

// Dial connects once. It fails if the guest agent is not listening yet.
func Dial(ctx context.Context, udsPath string, port uint32) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: unexpected %q", line)
	}
	_ = conn.SetDeadline(time.Time{})
	return &Client{conn: conn, r: r}, nil
}

// WaitDial retries Dial until the agent answers a ping or ctx expires.
// Firecracker closes the connection when nothing listens on the guest port,
// so early attempts fail fast.
func WaitDial(ctx context.Context, udsPath string, port uint32) (*Client, error) {
	var last error
	for {
		attempt, cancel := context.WithTimeout(ctx, time.Second)
		c, err := Dial(attempt, udsPath, port)
		if err == nil {
			if _, err = c.Call(attempt, &proto.Request{Op: proto.OpPing}); err == nil {
				cancel()
				return c, nil
			}
			c.Close()
		}
		cancel()
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("guest agent not ready: %w (last error: %v)", ctx.Err(), last)
		case <-time.After(time.Millisecond):
		}
	}
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Call(ctx context.Context, req *proto.Request) (*proto.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	req.ID = c.next
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(dl)
		defer c.conn.SetDeadline(time.Time{})
	}
	if err := proto.Write(c.conn, req); err != nil {
		return nil, err
	}
	var resp proto.Response
	if err := proto.Read(c.r, &resp); err != nil {
		return nil, err
	}
	if resp.ID != req.ID {
		return nil, fmt.Errorf("response id %d for request %d", resp.ID, req.ID)
	}
	if !resp.OK {
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
}

// Exec runs argv in the guest and returns its result; a non-zero exit is not an error.
func (c *Client) Exec(ctx context.Context, argv ...string) (*proto.ExecResult, error) {
	resp, err := c.Call(ctx, &proto.Request{Op: proto.OpExec, Exec: &proto.ExecArgs{Argv: argv}})
	if err != nil {
		return nil, err
	}
	return resp.Exec, nil
}
