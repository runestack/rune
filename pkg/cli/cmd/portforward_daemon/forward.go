package portforwarddaemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
)

// startForward sets up listeners, opens a session, and spawns the
// runForward goroutine. Returns once listeners are bound; the long-
// lived work (accept loop + receive loop + retry) runs in the
// background until ctx is cancelled or the daemon shuts down.
func (d *Daemon) startForward(ctx context.Context, fwd *Forward) (*runningForward, error) {
	if len(fwd.Mappings) == 0 {
		return nil, errors.New("forward has no port mappings")
	}

	// Bind every listener up-front. Bind failures must surface to
	// the caller (CLI `-d` invocation) so the operator sees the
	// error immediately instead of trying to use a non-existent
	// port.
	listeners := make([]net.Listener, 0, len(fwd.Mappings))
	for _, m := range fwd.Mappings {
		ln, err := net.Listen("tcp", m.Local)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return nil, fmt.Errorf("bind %s: %w", m.Local, err)
		}
		listeners = append(listeners, ln)
	}

	rCtx, cancel := context.WithCancel(ctx)
	rf := &runningForward{
		fwd:      fwd,
		cancel:   cancel,
		doneCh:   make(chan struct{}),
		logLines: newLogRing(500),
	}

	go d.runForward(rCtx, rf, listeners)
	return rf, nil
}

// runForward is the long-lived per-forward goroutine. It owns the
// listeners, opens a gRPC session, and reconnects on stream error
// with exponential backoff up to a cap.
func (d *Daemon) runForward(ctx context.Context, rf *runningForward, listeners []net.Listener) {
	defer close(rf.doneCh)
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	rf.logLines.push(fmt.Sprintf("forward %s started", rf.fwd.ID))

	// Reconnect loop.
	backoff := time.Second
	const maxBackoff = 60 * time.Second
	const maxAttempts = 10

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}

		sess, err := d.openSession(ctx, rf.fwd)
		if err != nil {
			rf.logLines.push(fmt.Sprintf("open session: %v", err))
			rf.fwd.Status = StatusReconnecting
			rf.fwd.LastError = err.Error()
			_ = WriteForward(d.stateDir, rf.fwd)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		rf.fwd.Status = StatusActive
		rf.fwd.LastError = ""
		_ = WriteForward(d.stateDir, rf.fwd)
		rf.logLines.push("session ready")
		backoff = time.Second // reset

		err = d.runSession(ctx, rf, listeners, sess)
		_ = sess.Close()

		// Clean shutdown: ctx cancelled.
		if ctx.Err() != nil {
			rf.logLines.push("forward stopping (ctx done)")
			return
		}

		if err != nil {
			rf.logLines.push(fmt.Sprintf("session error: %v", err))
			rf.fwd.Status = StatusReconnecting
			rf.fwd.LastError = err.Error()
			_ = WriteForward(d.stateDir, rf.fwd)
		}

		// Backoff before reconnect.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	rf.fwd.Status = StatusFailed
	rf.fwd.LastError = "max reconnect attempts exceeded"
	_ = WriteForward(d.stateDir, rf.fwd)
	rf.logLines.push("forward failed: max reconnect attempts exceeded")
}

// openSession dials the API server and opens the StreamPortForward
// stream, returning once the server has sent the Ready frame.
func (d *Daemon) openSession(ctx context.Context, fwd *Forward) (*client.PortForwardSession, error) {
	cli, err := d.newClient()
	if err != nil {
		return nil, fmt.Errorf("api client: %w", err)
	}
	pf := client.NewPortForwardClient(cli)

	tgt := client.PortForwardTarget{
		Namespace: fwd.Namespace,
		Pin:       fwd.InstancePin,
	}
	if fwd.TargetKind == TargetInstance {
		tgt.InstanceID = fwd.Target
	} else {
		tgt.Service = fwd.Target
	}
	for _, m := range fwd.Mappings {
		tgt.Ports = append(tgt.Ports, m.Remote)
	}

	sess, ready, err := pf.Open(ctx, tgt)
	if err != nil {
		_ = cli.Close()
		return nil, err
	}
	_ = ready // logged via logRing on the Ready frame; the daemon doesn't surface it elsewhere.
	return sess, nil
}

// runSession drives the bidi stream until it errors or ctx is
// cancelled. Per-conn accept happens here so each retry binds a fresh
// router; the local listeners stay open across retries so the
// operator's clients don't get connection-refused during a reconnect.
func (d *Daemon) runSession(
	ctx context.Context,
	rf *runningForward,
	listeners []net.Listener,
	sess *client.PortForwardSession,
) error {
	router := newDaemonRouter()
	recvDone := make(chan error, 1)
	go func() {
		for {
			msg, err := sess.Recv()
			if err != nil {
				recvDone <- err
				return
			}
			router.dispatch(msg)
		}
	}()

	var acceptWG sync.WaitGroup
	acceptCtx, acceptCancel := context.WithCancel(ctx)
	defer acceptCancel()

	for i, ln := range listeners {
		acceptWG.Add(1)
		go func(ln net.Listener, m PortMapping) {
			defer acceptWG.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						rf.logLines.push(fmt.Sprintf("accept %s: %v", m.Local, err))
					}
					return
				}
				rf.logLines.push(fmt.Sprintf("accepted on %s -> :%d", m.Local, m.Remote))
				go d.proxyConn(acceptCtx, sess, router, c, m.Remote, rf.logLines)
			}
		}(ln, rf.fwd.Mappings[i])
	}

	var sessionErr error
	select {
	case <-ctx.Done():
		sessionErr = ctx.Err()
	case err := <-recvDone:
		if err != nil && err != io.EOF {
			sessionErr = err
		}
	}

	acceptCancel()
	router.closeAll()
	return sessionErr
}

func (d *Daemon) proxyConn(
	ctx context.Context,
	sess *client.PortForwardSession,
	router *daemonRouter,
	local net.Conn,
	remotePort uint32,
	logs *logRing,
) {
	defer local.Close()

	connID := router.next()
	dataCh, closeCh := router.register(connID)
	defer router.unregister(connID)

	if err := sess.SendOpen(connID, remotePort); err != nil {
		logs.push(fmt.Sprintf("open conn_id=%d: %v", connID, err))
		return
	}

	// remote → local
	pumpDone := make(chan struct{})

	// Close the local socket when the remote side closes the conn or
	// the session ends. Without this, the local→remote loop below stays
	// blocked in local.Read() — a server-sent Close would never reach
	// it (a `break` inside a select breaks the select, not the loop),
	// leaving the caller's connection hung open instead of closed.
	//
	// On remote-close we must wait for the remote→local pump to finish
	// first: the server sends the response Data frame and the Close
	// frame back-to-back (e.g. an HTTP "Connection: close" reply), so
	// closing local the instant closeCh fires races the pump and drops
	// the still-buffered response — the caller then sees an empty
	// reply. pumpDone is closed only once dataCh is fully drained, so
	// waiting on it guarantees the response is written first. ctx.Done
	// (session teardown) closes immediately — best effort.
	go func() {
		select {
		case <-closeCh:
			<-pumpDone
		case <-ctx.Done():
		}
		_ = local.Close()
	}()

	go func() {
		defer close(pumpDone)
		for {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-dataCh:
				if !ok {
					return
				}
				if _, err := local.Write(payload); err != nil {
					_ = sess.SendClose(connID, err.Error())
					return
				}
			}
		}
	}()

	// local → remote. Exits on read/send error; the watcher goroutine
	// above closes local on remote-close/ctx-done, which surfaces here
	// as a read error.
	buf := make([]byte, 16*1024)
	for {
		n, err := local.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			if sendErr := sess.SendData(connID, payload); sendErr != nil {
				break
			}
		}
		if err != nil {
			_ = sess.SendClose(connID, "")
			break
		}
	}
	<-pumpDone
}

// --- daemonRouter (parallel to the CLI's connRouter; kept separate to
// avoid cross-package import cycles) ---

type daemonRouter struct {
	mu     sync.Mutex
	nextID uint64
	conns  map[uint64]*routedConn
}

type routedConn struct {
	data  chan []byte
	close chan struct{}
	once  sync.Once
}

func newDaemonRouter() *daemonRouter {
	return &daemonRouter{conns: map[uint64]*routedConn{}}
}

func (r *daemonRouter) next() uint64 {
	return atomic.AddUint64(&r.nextID, 1)
}

func (r *daemonRouter) register(id uint64) (<-chan []byte, <-chan struct{}) {
	rc := &routedConn{
		data:  make(chan []byte, 32),
		close: make(chan struct{}),
	}
	r.mu.Lock()
	r.conns[id] = rc
	r.mu.Unlock()
	return rc.data, rc.close
}

func (r *daemonRouter) unregister(id uint64) {
	r.mu.Lock()
	rc, ok := r.conns[id]
	delete(r.conns, id)
	r.mu.Unlock()
	if ok {
		rc.once.Do(func() {
			close(rc.close)
			close(rc.data)
		})
	}
}

func (r *daemonRouter) dispatch(msg *generated.PortForwardServerMessage) {
	switch m := msg.GetMessage().(type) {
	case *generated.PortForwardServerMessage_Data:
		r.mu.Lock()
		rc, ok := r.conns[m.Data.ConnId]
		r.mu.Unlock()
		if !ok {
			return
		}
		select {
		case rc.data <- m.Data.Payload:
		case <-rc.close:
		}
	case *generated.PortForwardServerMessage_Close:
		r.mu.Lock()
		rc, ok := r.conns[m.Close.ConnId]
		r.mu.Unlock()
		if !ok {
			return
		}
		rc.once.Do(func() {
			close(rc.close)
			close(rc.data)
		})
	}
}

func (r *daemonRouter) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rc := range r.conns {
		rc.once.Do(func() {
			close(rc.close)
			close(rc.data)
		})
	}
	r.conns = map[uint64]*routedConn{}
}

// --- explicit logger interface used only to satisfy unused import in
// some configurations during refactors ---

var _ log.Logger
