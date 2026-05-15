package portforwarddaemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/log"
)

// IdleShutdownAfter is how long the daemon waits with zero forwards
// before exiting. Tunable for tests.
var IdleShutdownAfter = 60 * time.Second

// ClientFactory builds an API client for the daemon's outbound gRPC
// calls. Injectable for tests; production wires it to the same
// createAPIClient the CLI uses.
type ClientFactory func() (*client.Client, error)

// Daemon owns the unix socket listener, the active set of forwards,
// and the idle-shutdown timer.
type Daemon struct {
	stateDir  string
	socketLn  net.Listener
	logger    log.Logger
	newClient ClientFactory

	mu       sync.Mutex
	forwards map[string]*runningForward // by ID
	lastIdle time.Time                  // when forwards last hit zero

	stopCh chan struct{} // closed to ask the daemon to exit
}

// runningForward couples persisted state with the in-process objects
// that own the local listeners and the gRPC session.
type runningForward struct {
	fwd      *Forward
	cancel   context.CancelFunc
	doneCh   chan struct{}
	logLines *logRing
}

// New constructs a Daemon bound to the given state directory.
func New(stateDir string, logger log.Logger, newClient ClientFactory) *Daemon {
	return &Daemon{
		stateDir:  stateDir,
		logger:    logger,
		newClient: newClient,
		forwards:  map[string]*runningForward{},
		lastIdle:  time.Now(),
		stopCh:    make(chan struct{}),
	}
}

// Run binds the unix socket, writes the pid file, then services
// requests until ctx is cancelled, the idle timer fires, or Stop is
// called. Returns once everything is torn down.
func (d *Daemon) Run(ctx context.Context) error {
	pidPath := PidPath(d.stateDir)
	sockPath := SocketPath(d.stateDir)

	// Acquire the pid-file lock. flock prevents two daemons from
	// racing on the same state dir. If we lose the race, exit
	// cleanly — the lock-holder is authoritative.
	pidFile, err := os.OpenFile(pidPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open pid file: %w", err)
	}
	defer pidFile.Close()
	if err := syscall.Flock(int(pidFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another daemon is running: %w", err)
	}
	// Truncate + write our pid.
	_ = pidFile.Truncate(0)
	_, _ = pidFile.Seek(0, 0)
	fmt.Fprintf(pidFile, "%d\n", os.Getpid())

	// Bind the socket. Remove any stale file first; the flock above
	// guarantees no live daemon owns it.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0o700)
	d.socketLn = ln
	defer func() { _ = os.Remove(sockPath) }()

	d.logger.Info("port-forward daemon ready",
		log.Str("socket", sockPath),
		log.Int("pid", os.Getpid()))

	// Recovery: re-create persisted forwards.
	if err := d.recoverPersisted(ctx); err != nil {
		d.logger.Warn("port-forward recovery saw errors", log.Err(err))
	}

	// Accept loop.
	acceptDone := make(chan struct{})
	go d.acceptLoop(ctx, acceptDone)

	// Idle watchdog. Tick at most every 5s; for tiny
	// IdleShutdownAfter (tests) tick proportionally so the test
	// suite doesn't pay a 5-second worst case per case.
	tickEvery := 5 * time.Second
	if IdleShutdownAfter < tickEvery {
		tickEvery = IdleShutdownAfter / 2
		if tickEvery < 50*time.Millisecond {
			tickEvery = 50 * time.Millisecond
		}
	}
	idleTick := time.NewTicker(tickEvery)
	defer idleTick.Stop()

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("port-forward daemon: ctx cancelled")
			_ = ln.Close()
			<-acceptDone
			d.shutdownAllForwards()
			return nil

		case <-d.stopCh:
			d.logger.Info("port-forward daemon: stop requested")
			_ = ln.Close()
			<-acceptDone
			d.shutdownAllForwards()
			return nil

		case <-idleTick.C:
			d.mu.Lock()
			n := len(d.forwards)
			idleFor := time.Since(d.lastIdle)
			d.mu.Unlock()
			if n == 0 && idleFor > IdleShutdownAfter {
				d.logger.Info("port-forward daemon: idle, exiting",
					log.Str("idle_for", idleFor.String()))
				_ = ln.Close()
				<-acceptDone
				return nil
			}
		}
	}
}

func (d *Daemon) acceptLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	for {
		conn, err := d.socketLn.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			d.logger.Warn("accept failed", log.Err(err))
			return
		}
		go d.handleConn(ctx, conn)
	}
}

func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	var req Request
	if err := ReadJSONLine(br, &req); err != nil {
		_ = WriteJSONLine(conn, Response{OK: false, Error: err.Error()})
		return
	}
	resp := d.dispatch(ctx, &req)
	_ = WriteJSONLine(conn, resp)
}

func (d *Daemon) dispatch(ctx context.Context, req *Request) Response {
	switch req.Cmd {
	case CmdPing:
		return Response{OK: true, Version: "rune-pf-daemon/1"}
	case CmdAdd:
		return d.cmdAdd(ctx, req)
	case CmdList:
		return d.cmdList()
	case CmdStop:
		return d.cmdStop(req.ID)
	case CmdStopAll:
		return d.cmdStopAll()
	case CmdLogs:
		return d.cmdLogs(req.ID, req.Tail)
	default:
		return Response{OK: false, Error: fmt.Sprintf("unknown cmd %q", req.Cmd)}
	}
}

func (d *Daemon) cmdAdd(ctx context.Context, req *Request) Response {
	if req.Forward == nil {
		return Response{OK: false, Error: "add: missing forward"}
	}
	fwd := req.Forward
	if fwd.ID == "" {
		fwd.ID = newForwardID()
	}
	if fwd.CreatedAt.IsZero() {
		fwd.CreatedAt = time.Now().UTC()
	}
	fwd.Status = StatusActive
	fwd.LastError = ""

	rf, err := d.startForward(ctx, fwd)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}

	d.mu.Lock()
	d.forwards[fwd.ID] = rf
	d.mu.Unlock()

	if err := WriteForward(d.stateDir, fwd); err != nil {
		d.logger.Warn("persist forward failed", log.Err(err), log.Str("id", fwd.ID))
	}
	return Response{OK: true, Forward: fwd}
}

func (d *Daemon) cmdList() Response {
	d.mu.Lock()
	out := make([]*Forward, 0, len(d.forwards))
	for _, rf := range d.forwards {
		// Return a copy so callers can't mutate daemon state.
		fwdCopy := *rf.fwd
		out = append(out, &fwdCopy)
	}
	d.mu.Unlock()
	return Response{OK: true, Forwards: out}
}

func (d *Daemon) cmdStop(id string) Response {
	d.mu.Lock()
	rf, ok := d.forwards[id]
	if ok {
		delete(d.forwards, id)
	}
	idleNow := len(d.forwards) == 0
	d.mu.Unlock()

	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("no such forward: %s", id)}
	}
	rf.cancel()
	<-rf.doneCh
	_ = RemoveForward(d.stateDir, id)

	if idleNow {
		d.markIdle()
	}
	return Response{OK: true}
}

func (d *Daemon) cmdStopAll() Response {
	d.mu.Lock()
	toStop := make([]*runningForward, 0, len(d.forwards))
	for _, rf := range d.forwards {
		toStop = append(toStop, rf)
	}
	d.forwards = map[string]*runningForward{}
	d.mu.Unlock()

	for _, rf := range toStop {
		rf.cancel()
		<-rf.doneCh
		_ = RemoveForward(d.stateDir, rf.fwd.ID)
	}
	d.markIdle()
	return Response{OK: true, Stopped: len(toStop)}
}

func (d *Daemon) cmdLogs(id string, tail int) Response {
	d.mu.Lock()
	rf, ok := d.forwards[id]
	d.mu.Unlock()
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("no such forward: %s", id)}
	}
	if tail <= 0 {
		tail = 200
	}
	return Response{OK: true, Lines: rf.logLines.tail(tail)}
}

// markIdle stamps the time at which the forwards table became empty
// so the idle watchdog can decide when to exit.
func (d *Daemon) markIdle() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.forwards) == 0 {
		d.lastIdle = time.Now()
	}
}

// shutdownAllForwards is called during daemon teardown to cleanly
// close every running forward.
func (d *Daemon) shutdownAllForwards() {
	d.mu.Lock()
	rfs := make([]*runningForward, 0, len(d.forwards))
	for _, rf := range d.forwards {
		rfs = append(rfs, rf)
	}
	d.forwards = map[string]*runningForward{}
	d.mu.Unlock()
	for _, rf := range rfs {
		rf.cancel()
		<-rf.doneCh
	}
}

func (d *Daemon) recoverPersisted(ctx context.Context) error {
	fwds, err := LoadForwards(d.stateDir)
	if err != nil {
		return err
	}
	var failures []error
	for _, fwd := range fwds {
		rf, err := d.startForward(ctx, fwd)
		if err != nil {
			fwd.Status = StatusFailed
			fwd.LastError = err.Error()
			_ = WriteForward(d.stateDir, fwd)
			failures = append(failures, fmt.Errorf("%s: %w", fwd.ID, err))
			continue
		}
		d.mu.Lock()
		d.forwards[fwd.ID] = rf
		d.mu.Unlock()
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

// newForwardID returns a short, kebab-prefixed identifier.
func newForwardID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "fwd-" + hex.EncodeToString(b[:])
}

// --- logRing ---

// logRing is a fixed-size in-memory ring of log lines per forward.
type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLogRing(max int) *logRing { return &logRing{max: max} }

func (r *logRing) push(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, time.Now().UTC().Format(time.RFC3339)+" "+line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *logRing) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= len(r.lines) {
		out := make([]string, len(r.lines))
		copy(out, r.lines)
		return out
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// --- helpers used by spawn / status code ---

// IsAlive returns true if the pid in `daemon.pid` corresponds to a
// running process. Used by `list` for a quick "is the daemon up?"
// probe without opening a socket connection.
func IsAlive(stateDir string) bool {
	b, err := os.ReadFile(PidPath(stateDir))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(string(bytesTrimSpace(b)))
	if err != nil || pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 is a permission/existence probe.
	return p.Signal(syscall.Signal(0)) == nil
}

func bytesTrimSpace(b []byte) []byte {
	// avoid pulling in strings for a single-byte trim
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\r' || c == '\t' }

// ioCopyClose is a small helper for tests / future use that doesn't
// fit elsewhere; kept here to keep helpers in one place.
func ioCopyClose(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	return err
}
