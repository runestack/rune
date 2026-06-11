// Package alerting is the RuneSight alerter: it evaluates stored alert rules
// against the cluster's LogStore on a rolling cadence, drives the
// ok → pending → firing → resolved state machine, and notifies on transitions
// (a Rune event always; the rule's channels on firing/resolved).
//
// Rules are Core-tier by construction — a LogQL log selector counted over a
// window and compared to a threshold — so alerting behaves identically on the
// embedded, Loki, and ClickHouse backends. Absence/heartbeat alerts are the
// `== 0` (or `< 1`) case of the same shape.
//
// Status is held in memory: a restart re-evaluates every rule within one
// interval, which is the operationally correct recovery anyway. Transitions
// are also recorded as Rune events, so history survives in the event log.
package alerting

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/types"
)

const (
	// DefaultInterval is the evaluation cadence for rules that don't set one.
	DefaultInterval = 60 * time.Second

	// tick is how often the loop scans for due rules; per-rule Interval is
	// honoured on top of this resolution.
	tick = 15 * time.Second
)

// RuleSource lists the alert rules to evaluate (implemented by
// repos.AlertRuleRepo).
type RuleSource interface {
	List(ctx context.Context) ([]*types.AlertRule, error)
}

// ChannelSource resolves channel names (implemented by repos.ChannelRepo).
type ChannelSource interface {
	Get(ctx context.Context, name string) (*types.Channel, error)
}

// EventSink receives alert-transition events (implemented by events.EventLog).
type EventSink interface {
	Emit(ctx context.Context, e types.Event) error
}

// SecretLookup resolves ${secret:namespace/name/key} references in channel
// URLs and headers at send time. Mirrors driverparams.SecretLookup.
type SecretLookup func(ctx context.Context, namespace, name, key string) (string, error)

// Status is a rule's live evaluation state.
type Status struct {
	Rule  string
	State string // ok | pending | firing
	// Value is the last evaluated windowed count.
	Value float64
	// Since is when the current State began.
	Since time.Time
	// LastEval is the last evaluation time.
	LastEval time.Time
	// LastError is the last evaluation or delivery error ("" when healthy).
	LastError string
}

// Alerter evaluates rules and dispatches notifications.
type Alerter struct {
	store    observe.LogStore
	rules    RuleSource
	channels ChannelSource
	events   EventSink
	notifier *notifier
	logger   log.Logger
	now      func() time.Time

	mu     sync.RWMutex
	status map[string]*Status // by rule name

	stopOnce sync.Once
	stopCh   chan struct{}
	stopped  chan struct{}
}

// Options configures the Alerter. Store and Rules are required; the rest are
// optional (nil EventSink/SecretLookup degrade gracefully).
type Options struct {
	Store    observe.LogStore
	Rules    RuleSource
	Channels ChannelSource
	Events   EventSink
	Secrets  SecretLookup
	Logger   log.Logger
	// Now lets tests inject a clock.
	Now func() time.Time
}

// New constructs an Alerter. Call Start to begin the loop; Tick is exposed
// for tests and embedded callers.
func New(opts Options) *Alerter {
	logger := opts.Logger
	if logger == nil {
		logger = log.GetDefaultLogger()
	}
	logger = logger.WithComponent("observe.alerter")
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Alerter{
		store:    opts.Store,
		rules:    opts.Rules,
		channels: opts.Channels,
		events:   opts.Events,
		notifier: newNotifier(opts.Secrets, logger),
		logger:   logger,
		now:      now,
		status:   make(map[string]*Status),
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start runs the evaluation loop until Stop. Non-blocking.
func (a *Alerter) Start(ctx context.Context) {
	go func() {
		defer close(a.stopped)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-a.stopCh:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				a.Tick(ctx)
			}
		}
	}()
}

// Stop halts the loop. Safe to call multiple times.
func (a *Alerter) Stop() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
		<-a.stopped
	})
}

// Statuses returns a snapshot of every known rule status, sorted by rule name.
func (a *Alerter) Statuses() []Status {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Status, 0, len(a.status))
	for _, s := range a.status {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	return out
}

// Tick evaluates every due rule once. Exposed for tests.
func (a *Alerter) Tick(ctx context.Context) {
	rules, err := a.rules.List(ctx)
	if err != nil {
		a.logger.Warn("alerter: list rules failed", log.Err(err))
		return
	}
	now := a.now()

	live := make(map[string]bool, len(rules))
	for _, rule := range rules {
		live[rule.Name] = true
		if rule.Disabled {
			a.dropStatus(rule.Name)
			continue
		}
		interval := rule.Interval
		if interval <= 0 {
			interval = DefaultInterval
		}
		st := a.getStatus(rule.Name)
		if !st.LastEval.IsZero() && now.Sub(st.LastEval) < interval {
			continue // not due yet
		}
		a.evaluate(ctx, rule, now)
	}

	// Forget statuses for deleted rules.
	a.mu.Lock()
	for name := range a.status {
		if !live[name] {
			delete(a.status, name)
		}
	}
	a.mu.Unlock()
}

// evaluate runs one rule's query and advances its state machine.
func (a *Alerter) evaluate(ctx context.Context, rule *types.AlertRule, now time.Time) {
	value, err := a.windowedCount(ctx, rule, now)
	st := a.getStatus(rule.Name)

	a.mu.Lock()
	st.LastEval = now
	if err != nil {
		// Evaluation errors never change alert state (a flapping backend must
		// not resolve a real alert); they surface on the status row.
		st.LastError = err.Error()
		a.mu.Unlock()
		a.logger.Warn("alerter: evaluation failed", log.Str("rule", rule.Name), log.Err(err))
		return
	}
	st.LastError = ""
	st.Value = value
	prev := st.State
	next := nextState(prev, compare(rule.Op, value, rule.Threshold), st.Since, rule.For, now)
	if next != prev {
		st.State = next
		st.Since = now
	}
	snapshot := *st
	a.mu.Unlock()

	if next != prev {
		a.transition(ctx, rule, prev, snapshot)
	}
}

// windowedCount runs count_over_time(rule.LogQL [window]) over the trailing
// window and sums the buckets into one count.
func (a *Alerter) windowedCount(ctx context.Context, rule *types.AlertRule, now time.Time) (float64, error) {
	logql := fmt.Sprintf("count_over_time(%s [%s])", rule.LogQL, rule.Window)
	q, err := observe.ParseLogQL(logql, now.Add(-rule.Window), now, 0, false)
	if err != nil {
		return 0, fmt.Errorf("parse rule query: %w", err)
	}
	rs, err := a.store.Execute(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("execute rule query: %w", err)
	}
	defer rs.Close()
	var total float64
	for rs.Next(ctx) {
		total += rs.Sample().Value
	}
	if err := rs.Err(); err != nil {
		return 0, fmt.Errorf("rule query stream: %w", err)
	}
	return total, nil
}

// nextState advances the ok → pending → firing machine. `for` is the
// hysteresis: the condition must hold that long (measured from when pending
// began) before firing. Zero `for` fires immediately.
func nextState(prev string, condTrue bool, since time.Time, forDur time.Duration, now time.Time) string {
	if !condTrue {
		return types.AlertStateOK
	}
	switch prev {
	case types.AlertStateFiring:
		return types.AlertStateFiring
	case types.AlertStatePending:
		if now.Sub(since) >= forDur {
			return types.AlertStateFiring
		}
		return types.AlertStatePending
	default: // ok
		if forDur <= 0 {
			return types.AlertStateFiring
		}
		return types.AlertStatePending
	}
}

func compare(op string, value, threshold float64) bool {
	switch op {
	case ">":
		return value > threshold
	case ">=":
		return value >= threshold
	case "<":
		return value < threshold
	case "<=":
		return value <= threshold
	case "==":
		return value == threshold
	default:
		return false
	}
}

// transition emits the Rune event and, for firing/resolved edges, notifies
// the rule's channels.
func (a *Alerter) transition(ctx context.Context, rule *types.AlertRule, prev string, st Status) {
	a.logger.Info("alert transition",
		log.Str("rule", rule.Name), log.Str("from", prev), log.Str("to", st.State),
		log.Str("value", fmt.Sprintf("%g", st.Value)))

	if a.events != nil {
		level := types.EventLevelInfo
		if st.State == types.AlertStateFiring {
			level = types.EventLevelWarn
		}
		_ = a.events.Emit(ctx, types.Event{
			Kind:    "AlertRule",
			Name:    rule.Name,
			Level:   level,
			Reason:  "Alert" + titleCase(st.State),
			Message: transitionMessage(rule, prev, st),
		})
	}

	// Channels hear about pages and all-clears, not pending wobble.
	firingEdge := st.State == types.AlertStateFiring
	resolvedEdge := prev == types.AlertStateFiring && st.State == types.AlertStateOK
	if !firingEdge && !resolvedEdge {
		return
	}
	for _, name := range rule.Channels {
		ch, err := a.channels.Get(ctx, name)
		if err != nil {
			a.logger.Warn("alerter: channel not found", log.Str("rule", rule.Name), log.Str("channel", name), log.Err(err))
			continue
		}
		if err := a.notifier.send(ctx, ch, rule, prev, st); err != nil {
			a.logger.Warn("alerter: notify failed", log.Str("rule", rule.Name), log.Str("channel", name), log.Err(err))
			a.mu.Lock()
			if cur, ok := a.status[rule.Name]; ok {
				cur.LastError = fmt.Sprintf("notify %s: %v", name, err)
			}
			a.mu.Unlock()
		}
	}
}

func transitionMessage(rule *types.AlertRule, prev string, st Status) string {
	switch {
	case st.State == types.AlertStateFiring:
		return fmt.Sprintf("alert %s firing: count %g %s %g over %s", rule.Name, st.Value, rule.Op, rule.Threshold, rule.Window)
	case prev == types.AlertStateFiring && st.State == types.AlertStateOK:
		return fmt.Sprintf("alert %s resolved: count %g", rule.Name, st.Value)
	default:
		return fmt.Sprintf("alert %s %s (count %g)", rule.Name, st.State, st.Value)
	}
}

func (a *Alerter) getStatus(name string) *Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.status[name]
	if !ok {
		st = &Status{Rule: name, State: types.AlertStateOK, Since: a.now()}
		a.status[name] = st
	}
	return st
}

func (a *Alerter) dropStatus(name string) {
	a.mu.Lock()
	delete(a.status, name)
	a.mu.Unlock()
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}
