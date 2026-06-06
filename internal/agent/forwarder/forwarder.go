// Package forwarder is the agent's log forwarder subsystem (plan §4.1). It is a
// dual tap: per-running-instance workload logs (via the orchestrator's
// GetInstanceLogs, which fans out to runner.GetLogs) plus the agent's Outbox
// for system/subsystem events. Every record is stamped with NodeID and instance
// metadata, buffered through a durable on-disk spool for at-least-once delivery,
// and pushed to ingest.
//
// On single-node, ingest is an in-process call into the control plane's
// ObserveService (LogStore.Write). On multi-node the same Ingester interface is
// implemented by an RPC client. The subsystem is OFF unless [observability] is
// enabled in the runefile — cmd/runed only registers it when a backend is
// configured.
//
// The forwarder registers as an internal/agent.Subsystem so its lifecycle is
// governed by the agent alongside dataplane/dns/volumes/ingress.
package forwarder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/internal/agent/outbox"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// Defaults can be overridden via Config.
const (
	// DefaultFlushInterval is how often the spool is drained to the ingester.
	DefaultFlushInterval = 2 * time.Second

	// DefaultBatchSize caps how many records are pushed per flush.
	DefaultBatchSize = 500

	// DefaultPollInterval is how often the forwarder re-scans running
	// instances to start taps for newly-scheduled instances.
	DefaultPollInterval = 5 * time.Second

	// DefaultOutboxDrain caps how many outbox entries are pulled per tick.
	DefaultOutboxDrain = 1024
)

// Ingester is the sink the forwarder pushes batches to. On single-node it is
// the in-process ObserveService; on multi-node an RPC client. Write must be
// safe for concurrent callers and should be at-least-once friendly (the
// forwarder retries failed batches from the spool).
type Ingester interface {
	Ingest(ctx context.Context, records []observe.LogRecord) error
}

// LogSource is the forwarder's read-only view of running instances and their
// logs. The orchestrator satisfies it (ListRunningInstances + GetInstanceLogs).
type LogSource interface {
	ListRunningInstances(ctx context.Context, namespace string) ([]*types.Instance, error)
	GetInstanceLogs(ctx context.Context, namespace, instanceID string, opts types.LogOptions) (io.ReadCloser, error)
}

// Config bundles forwarder construction parameters.
type Config struct {
	// Source taps workload logs. Required.
	Source LogSource

	// Ingester is the batch sink. Required.
	Ingester Ingester

	// Outbox is the agent's system-event buffer. Optional; when nil the
	// event tap is disabled and only workload logs are forwarded.
	Outbox *outbox.Outbox

	// NodeID is stamped on every record. Required.
	NodeID string

	// Spool is the durable buffer for at-least-once delivery. When nil an
	// in-memory spool is used (records are lost on crash, but never block the
	// taps). Provide a disk-backed spool for production durability.
	Spool Spool

	// FlushInterval, BatchSize, PollInterval, OutboxDrain override the
	// package defaults when > 0.
	FlushInterval time.Duration
	BatchSize     int
	PollInterval  time.Duration
	OutboxDrain   int

	// Logger; defaults to the global logger with component "agent.forwarder".
	Logger log.Logger
}

// Subsystem is the forwarder, an internal/agent.Subsystem.
type Subsystem struct {
	cfg     Config
	log     log.Logger
	spool   Spool
	flushIv time.Duration
	batchN  int
	pollIv  time.Duration
	drainN  int

	readyCh chan struct{}

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// taps tracks instances we have an active log tap for, so the poll loop
	// does not start a second reader for the same instance.
	tapsMu sync.Mutex
	taps   map[string]context.CancelFunc
}

// New constructs the forwarder subsystem.
func New(cfg Config) (*Subsystem, error) {
	if cfg.Source == nil {
		return nil, errors.New("forwarder: nil LogSource")
	}
	if cfg.Ingester == nil {
		return nil, errors.New("forwarder: nil Ingester")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("forwarder: empty NodeID")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger().WithComponent("agent.forwarder")
	}
	spool := cfg.Spool
	if spool == nil {
		spool = NewMemSpool(0)
	}
	s := &Subsystem{
		cfg:     cfg,
		log:     cfg.Logger,
		spool:   spool,
		flushIv: orDurationDefault(cfg.FlushInterval, DefaultFlushInterval),
		batchN:  orIntDefault(cfg.BatchSize, DefaultBatchSize),
		pollIv:  orDurationDefault(cfg.PollInterval, DefaultPollInterval),
		drainN:  orIntDefault(cfg.OutboxDrain, DefaultOutboxDrain),
		readyCh: make(chan struct{}),
		taps:    map[string]context.CancelFunc{},
	}
	return s, nil
}

// Name identifies the subsystem in agent logs.
func (s *Subsystem) Name() string { return "forwarder" }

// Ready closes once the forwarder's loops are running.
func (s *Subsystem) Ready() <-chan struct{} { return s.readyCh }

// Start brings up the poll loop (tapping new instances), the outbox tap, and
// the flush loop. Returns promptly; long-running work runs in goroutines.
func (s *Subsystem) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("forwarder: already started")
	}
	s.started = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(3)
	go s.pollLoop(runCtx)
	go s.outboxLoop(runCtx)
	go s.flushLoop(runCtx)

	close(s.readyCh)
	s.log.Info("forwarder started",
		log.Str("node_id", s.cfg.NodeID),
		log.Duration("flush_interval", s.flushIv),
		log.Int("batch_size", s.batchN),
	)
	return nil
}

// Stop cancels all loops and waits for them to drain (bounded by ctx).
func (s *Subsystem) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	// Best-effort final flush so buffered records aren't lost on graceful
	// shutdown.
	s.flushOnce(context.Background())
	s.log.Info("forwarder stopped")
	return nil
}

// pollLoop periodically scans running instances and starts a log tap for any
// instance not already tapped.
func (s *Subsystem) pollLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.pollIv)
	defer t.Stop()
	s.reconcileTaps(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcileTaps(ctx)
		}
	}
}

func (s *Subsystem) reconcileTaps(ctx context.Context) {
	instances, err := s.cfg.Source.ListRunningInstances(ctx, "")
	if err != nil {
		s.log.Debug("forwarder: list running instances failed", log.Err(err))
		return
	}
	live := map[string]struct{}{}
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		live[inst.ID] = struct{}{}
		s.ensureTap(ctx, inst)
	}
	// Drop taps for instances that are no longer running.
	s.tapsMu.Lock()
	for id, cancel := range s.taps {
		if _, ok := live[id]; !ok {
			cancel()
			delete(s.taps, id)
		}
	}
	s.tapsMu.Unlock()
}

// ensureTap starts a log reader for inst if not already running.
func (s *Subsystem) ensureTap(ctx context.Context, inst *types.Instance) {
	s.tapsMu.Lock()
	if _, ok := s.taps[inst.ID]; ok {
		s.tapsMu.Unlock()
		return
	}
	tapCtx, cancel := context.WithCancel(ctx)
	s.taps[inst.ID] = cancel
	s.tapsMu.Unlock()

	s.wg.Add(1)
	go s.tapInstance(tapCtx, inst)
}

// tapInstance follows one instance's logs and spools every line as a record.
func (s *Subsystem) tapInstance(ctx context.Context, inst *types.Instance) {
	defer s.wg.Done()
	defer func() {
		s.tapsMu.Lock()
		delete(s.taps, inst.ID)
		s.tapsMu.Unlock()
	}()

	reader, err := s.cfg.Source.GetInstanceLogs(ctx, inst.Namespace, inst.ID, types.LogOptions{
		Follow:     true,
		ShowLogs:   true,
		Timestamps: true,
	})
	if err != nil {
		s.log.Debug("forwarder: get instance logs failed",
			log.Str("instance", inst.ID), log.Err(err))
		return
	}
	defer reader.Close()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		s.spool.Push(s.recordFromInstanceLine(inst, line))
	}
}

// recordFromInstanceLine builds an enriched LogRecord from a raw workload log
// line, stripping the MultiLogStreamer metadata prefix if present.
func (s *Subsystem) recordFromInstanceLine(inst *types.Instance, line string) observe.LogRecord {
	_, _, tsStr, content := utils.ExtractLineMetadata(line)
	ts := time.Now().UTC()
	if tsStr != "" {
		if parsed, err := time.Parse("2006-01-02T15:04:05.000Z", tsStr); err == nil {
			ts = parsed
		}
	}
	stream := "stdout"
	level := classifyLevel(content)
	if level == "error" {
		stream = "stderr"
	}
	return observe.LogRecord{
		Timestamp: ts,
		Line:      content,
		Stream:    stream,
		Level:     level,
		Namespace: inst.Namespace,
		Service:   inst.ServiceName,
		Instance:  inst.ID,
		Node:      s.cfg.NodeID,
		// User labels denormalized from the parent service (e.g. app, tier),
		// so workload logs are queryable as {app="web"}. Read-only shared
		// reference — the instance's label map is immutable after creation.
		Labels: inst.Labels,
	}
}

// outboxLoop drains the agent outbox on a tick and spools each entry as a
// system-event record (Stream="event").
func (s *Subsystem) outboxLoop(ctx context.Context) {
	defer s.wg.Done()
	if s.cfg.Outbox == nil {
		return
	}
	t := time.NewTicker(s.flushIv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final drain so shutdown events are not lost.
			s.drainOutbox()
			return
		case <-t.C:
			s.drainOutbox()
		}
	}
}

func (s *Subsystem) drainOutbox() {
	entries := s.cfg.Outbox.Drain(s.drainN)
	for _, e := range entries {
		s.spool.Push(s.recordFromOutbox(e))
	}
}

func (s *Subsystem) recordFromOutbox(e outbox.Entry) observe.LogRecord {
	level := "info"
	if e.Kind == outbox.KindEvent {
		level = "event"
	}
	labels := map[string]string{}
	for k, v := range e.Fields {
		labels[k] = toStr(v)
	}
	if e.Source != "" {
		labels["source"] = e.Source
	}
	return observe.LogRecord{
		Timestamp: e.Timestamp,
		Line:      e.Message,
		Stream:    "event",
		Level:     level,
		Namespace: types.DefaultNamespace,
		Service:   e.Source,
		Node:      s.cfg.NodeID,
		Labels:    labels,
	}
}

// flushLoop periodically drains the spool to the ingester.
func (s *Subsystem) flushLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.flushIv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flushOnce(ctx)
		}
	}
}

// flushOnce drains up to BatchSize records and pushes them. On ingest failure
// the batch is returned to the spool (at-least-once: better a duplicate than a
// drop).
func (s *Subsystem) flushOnce(ctx context.Context) {
	batch := s.spool.Pop(s.batchN)
	if len(batch) == 0 {
		return
	}
	if err := s.cfg.Ingester.Ingest(ctx, batch); err != nil {
		s.log.Warn("forwarder: ingest failed; re-spooling batch",
			log.Int("records", len(batch)), log.Err(err))
		s.spool.Requeue(batch)
		return
	}
}

func classifyLevel(content string) string {
	lc := strings.ToLower(content)
	switch {
	case strings.Contains(lc, "error") || strings.Contains(lc, "exception") || strings.Contains(lc, "fatal"):
		return "error"
	case strings.Contains(lc, "warn"):
		return "warn"
	default:
		return "info"
	}
}

func toStr(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

func orDurationDefault(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}

func orIntDefault(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}
