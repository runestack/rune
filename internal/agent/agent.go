// Package agent is the per-node Rune daemon.
//
// On single-node, the agent runs in-process inside `runed`. On multi-node,
// the same agent code runs as a separate process on every worker node and
// talks to the control plane over gRPC. The single-node case is the
// simplest instantiation: the agent shares the control plane's process,
// store, and OrderedLog instance directly.
//
// The agent owns the node-local data plane: ordered watch consumer,
// userspace proxy + nftables (RUNE-041), embedded DNS (RUNE-063),
// network policy enforcement (RUNE-064), ingress controller (RUNE-066).
// This package, RUNE-032, ships only the lifecycle scaffold; the
// subsystems plug in as concrete Subsystem implementations in subsequent
// tickets.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/runestack/rune/internal/agent/outbox"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/orderedlog"
)

// Identity is the agent's stable node identity. Persisted on disk so
// that a process restart does not change the node ID.
type Identity struct {
	NodeID   string            `json:"node_id"`
	Hostname string            `json:"hostname"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// Mode tags the agent's deployment context. Used by subsystems to
// branch behavior (e.g. dev-mode skips nftables).
type Mode string

const (
	// ModeProduction is the normal single-node or multi-node case.
	ModeProduction Mode = "production"
	// ModeDev is the laptop / `--dev-mode` escape hatch. Subsystems MUST
	// honor this: nftables off, DNS resolves .rune to 127.0.0.1, ingress
	// can run on alternate ports, etc. See RUNE-041 / RUNE-063.
	ModeDev Mode = "dev"
)

// Config is the dependencies and tunables an Agent needs to run. Fields
// marked Required must be non-zero / non-nil; New returns an error
// otherwise.
type Config struct {
	// Identity is the persisted node identity. Required.
	Identity Identity

	// OrderedLog is the seam to control-plane state. On single-node this
	// is the in-process orderedlog.BadgerBackend; on multi-node it is a
	// thin RPC client over the watch stream (RUNE-028). Required.
	OrderedLog orderedlog.OrderedLog

	// Mode selects production vs dev behavior. Defaults to ModeProduction.
	Mode Mode

	// Logger is the structured logger to use. Defaults to the global
	// "agent" logger.
	Logger log.Logger

	// OutboxCapacity caps the in-memory outbox before back-pressure /
	// drop. Defaults to 4096.
	OutboxCapacity int

	// ReadyTimeout caps how long Start blocks waiting for every
	// registered subsystem to report ready. Defaults to 30s.
	ReadyTimeout time.Duration
}

func (c *Config) defaults() {
	if c.Mode == "" {
		c.Mode = ModeProduction
	}
	if c.Logger == nil {
		c.Logger = log.GetDefaultLogger().WithComponent("agent")
	}
	if c.OutboxCapacity <= 0 {
		c.OutboxCapacity = 4096
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = 30 * time.Second
	}
}

// Subsystem is a node-local component the Agent owns: data plane,
// DNS, policy enforcement, ingress, etc. Each ticket in the networking
// layer adds one.
//
// Lifecycle contract:
//   - Name returns a stable identifier for logs and metrics.
//   - Start MUST return promptly. Long-running work belongs in a
//     goroutine the subsystem owns. Start returning nil means the
//     subsystem accepted ownership; readiness is reported separately
//     via the ready channel.
//   - Ready returns a channel closed when the subsystem is ready to
//     serve traffic. The agent waits for all Ready channels (up to
//     ReadyTimeout) before reporting itself ready.
//   - Stop blocks until the subsystem has fully released resources.
//     Subsystems MUST be safe to Stop after a failed Start.
type Subsystem interface {
	Name() string
	Start(ctx context.Context) error
	Ready() <-chan struct{}
	Stop(ctx context.Context) error
}

// Agent is the per-node Rune daemon. It owns the lifecycle of a set of
// Subsystems, an outbox for buffered events/logs, and the agent's view
// of identity and mode.
type Agent struct {
	cfg Config
	log log.Logger

	subsMu sync.Mutex
	subs   []Subsystem

	outbox *outbox.Outbox

	startMu sync.Mutex
	started bool
	stopped bool

	// readyCh is closed once every subsystem reports ready or
	// ReadyTimeout elapses (in which case readyErr is set).
	readyCh  chan struct{}
	readyErr error
}

// New constructs an Agent. It does not start anything; call Start.
func New(cfg Config) (*Agent, error) {
	if cfg.Identity.NodeID == "" {
		return nil, errors.New("agent: empty NodeID in Identity")
	}
	if cfg.OrderedLog == nil {
		return nil, errors.New("agent: nil OrderedLog")
	}
	cfg.defaults()
	a := &Agent{
		cfg:     cfg,
		log:     cfg.Logger.With(log.Str("node_id", cfg.Identity.NodeID)),
		outbox:  outbox.New(cfg.OutboxCapacity, cfg.Logger.WithComponent("agent.outbox")),
		readyCh: make(chan struct{}),
	}
	return a, nil
}

// Identity returns the agent's identity. Safe to call any time.
func (a *Agent) Identity() Identity { return a.cfg.Identity }

// Mode returns the agent's mode. Safe to call any time.
func (a *Agent) Mode() Mode { return a.cfg.Mode }

// Outbox returns the agent's local outbox. Subsystems use this to
// queue logs and events that should be flushed to remote sinks
// (RuneSight, CloudWatch, etc.) in the background. The outbox is
// in-memory only in RUNE-032; persistence comes later.
func (a *Agent) Outbox() *outbox.Outbox { return a.outbox }

// Register attaches a Subsystem to the agent. It MUST be called before
// Start. Returns an error after Start has been called.
func (a *Agent) Register(s Subsystem) error {
	if s == nil {
		return errors.New("agent: nil subsystem")
	}
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if a.started {
		return errors.New("agent: cannot Register after Start")
	}
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	for _, existing := range a.subs {
		if existing.Name() == s.Name() {
			return fmt.Errorf("agent: subsystem %q already registered", s.Name())
		}
	}
	a.subs = append(a.subs, s)
	return nil
}

// Start brings up every registered subsystem in registration order
// and then waits (up to ReadyTimeout) for all of them to report ready.
// Returns nil when the agent is fully ready, or an error if any
// subsystem fails to start; subsystems already started are stopped on
// error so the agent leaves no orphaned goroutines behind.
//
// Start is non-blocking once it returns: subsystem long-running work
// runs in goroutines the subsystems own. Call Stop to shut down.
func (a *Agent) Start(ctx context.Context) error {
	a.startMu.Lock()
	if a.started {
		a.startMu.Unlock()
		return errors.New("agent: already started")
	}
	a.started = true
	a.startMu.Unlock()

	a.log.Info("agent starting",
		log.Str("mode", string(a.cfg.Mode)),
		log.Int("subsystems", len(a.subs)),
	)

	a.subsMu.Lock()
	subs := append([]Subsystem(nil), a.subs...)
	a.subsMu.Unlock()

	started := make([]Subsystem, 0, len(subs))
	for _, s := range subs {
		if err := s.Start(ctx); err != nil {
			a.log.Error("subsystem failed to start",
				log.Str("subsystem", s.Name()), log.Err(err))
			// Roll back: stop the ones we already started.
			a.shutdown(context.Background(), started)
			return fmt.Errorf("agent: subsystem %q start: %w", s.Name(), err)
		}
		a.log.Debug("subsystem started", log.Str("subsystem", s.Name()))
		started = append(started, s)
	}

	go a.waitReady(subs)

	return nil
}

// Ready returns a channel that is closed once every subsystem has
// reported ready, or once ReadyTimeout has elapsed (check ReadyErr in
// that case). For agents with no subsystems, Ready closes immediately
// after Start.
func (a *Agent) Ready() <-chan struct{} { return a.readyCh }

// ReadyErr returns the error (if any) that prevented full readiness.
// nil means every subsystem reported ready before ReadyTimeout.
// Call only after Ready() has closed.
func (a *Agent) ReadyErr() error { return a.readyErr }

func (a *Agent) waitReady(subs []Subsystem) {
	if len(subs) == 0 {
		close(a.readyCh)
		a.log.Info("agent ready (no subsystems)")
		return
	}
	deadline := time.NewTimer(a.cfg.ReadyTimeout)
	defer deadline.Stop()
	pending := make(map[string]<-chan struct{}, len(subs))
	for _, s := range subs {
		pending[s.Name()] = s.Ready()
	}
	for len(pending) > 0 {
		// Build a select dynamically would require reflect; instead,
		// drive one subsystem per loop iteration by ranging.
		nextName, nextCh := pickPending(pending)
		select {
		case <-nextCh:
			delete(pending, nextName)
			a.log.Debug("subsystem ready", log.Str("subsystem", nextName))
		case <-deadline.C:
			names := make([]string, 0, len(pending))
			for n := range pending {
				names = append(names, n)
			}
			a.readyErr = fmt.Errorf("agent: ready timeout; pending=%v", names)
			a.log.Warn("agent ready timeout",
				log.Any("pending_subsystems", names),
				log.Duration("timeout", a.cfg.ReadyTimeout),
			)
			close(a.readyCh)
			return
		}
	}
	close(a.readyCh)
	a.log.Info("agent ready")
}

// pickPending returns one entry from the map. Deterministic order is
// not required; we just need *some* channel to wait on. With a small
// number of subsystems, this is fine.
func pickPending(m map[string]<-chan struct{}) (string, <-chan struct{}) {
	for k, v := range m {
		return k, v
	}
	return "", nil
}

// Stop shuts every subsystem down in reverse registration order. Safe
// to call multiple times; only the first call does work. Safe to call
// before Start (no-op).
func (a *Agent) Stop(ctx context.Context) error {
	a.startMu.Lock()
	if !a.started || a.stopped {
		a.stopped = true
		a.startMu.Unlock()
		return nil
	}
	a.stopped = true
	a.startMu.Unlock()

	a.subsMu.Lock()
	subs := append([]Subsystem(nil), a.subs...)
	a.subsMu.Unlock()

	a.shutdown(ctx, subs)
	a.outbox.Close()
	a.log.Info("agent stopped")
	return nil
}

// shutdown stops the given subsystems in reverse order, logging any
// errors but never panicking.
func (a *Agent) shutdown(ctx context.Context, subs []Subsystem) {
	for i := len(subs) - 1; i >= 0; i-- {
		s := subs[i]
		if err := s.Stop(ctx); err != nil {
			a.log.Warn("subsystem stop error",
				log.Str("subsystem", s.Name()), log.Err(err))
		}
	}
}
