// Package nodeinfo is the per-node Subsystem that persists this
// machine's inventory record — what hardware it has, written by its own
// agent.
//
// It writes exactly one row, keyed by the agent's node identity under an
// empty namespace. On a machine with no GPU that row carries an empty
// device list, which is the normal answer and not an error.
//
// Two commitments are load-bearing and easy to break by accident:
//
//   - NOTHING PERIODIC. The probe runs once per agent start and never
//     again. Hotplug is out of scope (these are servers); a re-probe means
//     a restart. Adding a ticker "so it picks up a card later" would put a
//     recurring wakeup on every GPU-less machine in the fleet, which is
//     the cost this design exists to avoid. no_ticker_test.go trips on the
//     shapes people actually write; it is a tripwire, not a proof.
//
//   - THE PROBE NEVER BLOCKS OR FAILS runed's BOOT. Agent.Start runs each
//     subsystem's Start serially with no timeout and treats any error as
//     fatal, and a process wedged in an uninterruptible driver ioctl
//     cannot be killed — so exec.CommandContext does NOT bound it.
//     Start therefore launches the probe in its own goroutine and
//     returns; Ready closes on the first result or a deadline, whichever
//     comes first; a probe past the deadline is ABANDONED, not joined,
//     and records itself in DeviceProbeError. The services on the box must
//     not stay down for a GPU they do not use.
package nodeinfo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// DefaultProbeDeadline caps how long Ready() waits for the first probe
// result. Past it the agent reports ready anyway and the record carries
// a probe error, because a stuck driver must not hold up the daemon.
const DefaultProbeDeadline = 10 * time.Second

// LocalNodeAddress is the Address written for the node this agent runs
// on. types.Node.Validate() requires a non-empty address and a single-node
// install has no meaningful routable value; defining one here keeps the
// write on the validated path instead of routing around it.
const LocalNodeAddress = "127.0.0.1"

// Config is what the subsystem needs. Store and NodeID are required.
type Config struct {
	// Repo persists the node record. Required.
	Repo *repos.NodeRepo

	// Ledger, when set, gets an empty row created alongside the node
	// record. The orchestrator's reservation CAS reads-modifies-writes an
	// existing key and fails not-found on an absent one, so without this
	// the first GPU reservation on a box fails with a raw store error
	// instead of an admission decision. Optional and nil-safe.
	Ledger *repos.NodeLedgerRepo

	// NodeID is this machine's identity: the same ID Volume.BoundNode and
	// the observability stream label use. Required.
	NodeID string

	// Labels are the agent identity's labels, copied onto the record.
	Labels map[string]string

	// Provider probes for devices. Defaults to NullProvider().
	Provider DeviceProvider

	// Events is the persisted event log. Optional and nil-safe: when
	// wired, a re-probe whose device set changed emits a Node event so
	// `rune describe node` shows the transition. There is no re-probe
	// loop, so this fires at most once per agent start.
	Events events.EventLog

	// ProbeDeadline overrides DefaultProbeDeadline. Test seam.
	ProbeDeadline time.Duration

	// Logger is the structured logger. Required.
	Logger log.Logger
}

// Subsystem persists this node's inventory record.
type Subsystem struct {
	cfg Config
	log log.Logger

	readyCh   chan struct{}
	readyOnce sync.Once

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
}

// New constructs the subsystem.
func New(cfg Config) (*Subsystem, error) {
	if cfg.Repo == nil {
		return nil, errors.New("agent.nodeinfo: nil Repo")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("agent.nodeinfo: empty NodeID")
	}
	if cfg.Provider == nil {
		cfg.Provider = NullProvider()
	}
	if cfg.ProbeDeadline <= 0 {
		cfg.ProbeDeadline = DefaultProbeDeadline
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("agent.nodeinfo")
	}
	return &Subsystem{
		cfg:     cfg,
		log:     logger.With(log.Str("node_id", cfg.NodeID)),
		readyCh: make(chan struct{}),
	}, nil
}

// Name implements agent.Subsystem.
func (s *Subsystem) Name() string { return "agent.nodeinfo" }

// Ready closes once the node record has been written, or once the probe
// deadline elapses — whichever comes first.
func (s *Subsystem) Ready() <-chan struct{} { return s.readyCh }

// Start launches the probe-and-write in its own goroutine and returns
// immediately. It never fails on a probe error: a wedged driver must not
// keep the daemon down.
func (s *Subsystem) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("agent.nodeinfo: Start called twice")
	}
	if s.stopped {
		s.mu.Unlock()
		return errors.New("agent.nodeinfo: Start after Stop")
	}
	s.started = true
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	go s.probeAndWrite(runCtx)
	return nil
}

// Stop releases the subsystem. A probe goroutine still running past the
// deadline is left to finish on its own — see the package comment.
func (s *Subsystem) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	return nil
}

// probeAndWrite runs the probe under a deadline and persists whatever it
// learned. Both outcomes — devices or an error — end in a written record,
// because every diagnosis an operator gets is assembled from the record,
// not from logs.
func (s *Subsystem) probeAndWrite(ctx context.Context) {
	defer s.readyOnce.Do(func() { close(s.readyCh) })

	devices, probeErr := s.probe(ctx)
	if ctx.Err() != nil {
		// Shutting down. Writing a record with a cancelled context would
		// only produce a misleading warning on the way out.
		return
	}

	probedAt := time.Now()
	node := &types.Node{
		ID:              s.cfg.NodeID,
		Name:            s.cfg.NodeID,
		Address:         LocalNodeAddress,
		Labels:          copyLabels(s.cfg.Labels),
		Devices:         devices,
		DevicesProbedAt: &probedAt,
	}
	if probeErr != nil {
		node.DeviceProbeError = probeErr.Error()
	}

	// The device set is compared by UUID rather than by a version counter:
	// types.Node has no such field, and the UUID set is the thing that
	// actually changed. Read before the write, since Upsert replaces the
	// row.
	previous, prevErr := s.cfg.Repo.Get(ctx, s.cfg.NodeID)
	if prevErr != nil {
		previous = nil
	}

	switch {
	case probeErr != nil:
		// A probe that could not answer IS worth a warning: a driver
		// that broke overnight is otherwise invisible until someone
		// tries to use the GPU.
		s.log.Warn("device probe failed",
			log.Str("provider", s.cfg.Provider.Name()), log.Err(probeErr))
	case len(devices) == 0:
		// Graceful absence is the contract. ONE debug line, never warn —
		// a GPU-less box has nothing to warn about.
		s.log.Debug("no devices found",
			log.Str("provider", s.cfg.Provider.Name()))
	default:
		s.log.Info("device inventory probed",
			log.Str("provider", s.cfg.Provider.Name()),
			log.Int("devices", len(devices)))
	}

	// Before the record, so nothing can observe an inventory without a
	// ledger to reserve against.
	if s.cfg.Ledger != nil {
		if err := s.cfg.Ledger.EnsureExists(ctx, s.cfg.NodeID); err != nil {
			s.log.Warn("failed to create node device ledger", log.Err(err))
		}
	}

	if err := s.cfg.Repo.Upsert(ctx, node); err != nil {
		// The record is diagnostics, never the correctness path: a
		// failed write must not take the agent down with it.
		s.log.Warn("failed to write node record", log.Err(err))
		return
	}
	s.emitDeviceSetChange(ctx, previous, node)
}

// emitDeviceSetChange fires a Node event when a re-probe found a
// different set of device UUIDs than the last one did. First boot is not
// a change — there is nothing to have changed from.
func (s *Subsystem) emitDeviceSetChange(ctx context.Context, previous, current *types.Node) {
	if s.cfg.Events == nil || previous == nil {
		return
	}
	before, after := deviceUUIDs(previous.Devices), deviceUUIDs(current.Devices)
	if equalUUIDSets(before, after) {
		return
	}

	// Both slugs come from the fixed event vocabulary this design owns;
	// inventing a new one here would fork it for every consumer that
	// scripts against these. Capacity going down is the direction that
	// strands a workload, so it reads louder than capacity going up.
	level, reason := types.EventLevelInfo, "GpuInventoryProbed"
	if len(after) < len(before) {
		level, reason = types.EventLevelWarn, "GpuCapacityShrunk"
	}
	message := fmt.Sprintf("device set changed on re-probe: was [%s], now [%s]",
		strings.Join(before, ", "), strings.Join(after, ", "))

	if err := s.cfg.Events.Emit(ctx, types.Event{
		// Cluster-scoped, like the record itself.
		Namespace: "",
		Kind:      "Node",
		Name:      current.Name,
		UID:       current.ID,
		Level:     level,
		Reason:    reason,
		Message:   message,
	}); err != nil {
		s.log.Warn("failed to emit node event", log.Err(err))
	}
}

func deviceUUIDs(devices []types.GPUDevice) []string {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.UUID)
	}
	sort.Strings(out)
	return out
}

func equalUUIDSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// probe runs Provider.Probe in a goroutine and gives up on it after the
// deadline. The goroutine is deliberately ABANDONED rather than joined:
// a process blocked in a driver ioctl cannot be interrupted, so waiting
// for it would be waiting forever. The accepted cost is one leaked
// goroutine for the lifetime of the process on a wedged driver — which
// is strictly better than a daemon that will not boot.
func (s *Subsystem) probe(ctx context.Context) ([]types.GPUDevice, error) {
	type result struct {
		devices []types.GPUDevice
		err     error
	}
	// Buffered so the abandoned goroutine can always finish its send.
	done := make(chan result, 1)
	go func() {
		devices, err := s.cfg.Provider.Probe(ctx)
		done <- result{devices: devices, err: err}
	}()

	timer := time.NewTimer(s.cfg.ProbeDeadline)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.devices, r.err
	case <-timer.C:
		return nil, fmt.Errorf("probe timed out after %s", s.cfg.ProbeDeadline)
	case <-ctx.Done():
		return nil, fmt.Errorf("probe cancelled: %w", ctx.Err())
	}
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
