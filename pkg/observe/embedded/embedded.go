// Package embedded implements observe.LogStore as a lightweight in-process
// store that ships in runed by default. It is Core-tier: label match, line
// substring/regex grep, and count_over_time histograms (optionally grouped by
// a label dimension). It backs "every cluster gets persistent logs with zero
// extra services" (plan §2, §4.3).
//
// The MVP keeps records in a bounded in-memory ring with a background retention
// sweeper. It is intentionally simple and lives entirely behind the
// observe.LogStore interface, so a purpose-built segment store (or the existing
// Badger store) can replace it later without touching any caller. See plan §8
// (open question: embedded store engine).
package embedded

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
)

// Compile-time assertion that Store satisfies the seam.
var _ observe.LogStore = (*Store)(nil)

// Defaults can be overridden via Config.
const (
	// DefaultMaxRecords bounds the in-memory ring. On overflow the oldest
	// records are evicted first (drop-oldest), mirroring the Outbox policy.
	DefaultMaxRecords = 500_000

	// DefaultRetention is the maximum age a record is kept before the sweeper
	// evicts it. Matches the runefile default of 7 days.
	DefaultRetention = 7 * 24 * time.Hour

	// DefaultQueryLimit caps returned log lines when a Query sets Limit=0.
	DefaultQueryLimit = 1000

	// defaultSweepInterval is how often the retention sweeper runs.
	defaultSweepInterval = time.Minute
)

// Config configures the embedded store.
type Config struct {
	// MaxRecords bounds the in-memory ring. Zero uses DefaultMaxRecords.
	MaxRecords int

	// Retention is the max record age. Zero uses DefaultRetention; negative
	// disables age-based eviction (only the size bound applies).
	Retention time.Duration

	// Logger; defaults to the global logger with component "observe.embedded".
	Logger log.Logger

	// now lets tests inject a clock. nil uses time.Now.
	now func() time.Time
}

// Store is the embedded, in-process observe.LogStore.
type Store struct {
	maxRecords int
	retention  time.Duration
	log        log.Logger
	now        func() time.Time

	mu  sync.RWMutex
	buf []observe.LogRecord // append-ordered (ingest order ≈ time order)

	sweepOnce sync.Once
	stopCh    chan struct{}
	stopped   chan struct{}
}

// New constructs an embedded store and starts its retention sweeper. Call Close
// to stop the sweeper.
func New(cfg Config) *Store {
	if cfg.MaxRecords <= 0 {
		cfg.MaxRecords = DefaultMaxRecords
	}
	if cfg.Retention == 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger().WithComponent("observe.embedded")
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	s := &Store{
		maxRecords: cfg.MaxRecords,
		retention:  cfg.Retention,
		log:        cfg.Logger,
		now:        cfg.now,
		buf:        make([]observe.LogRecord, 0, 1024),
		stopCh:     make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	go s.sweepLoop()
	return s
}

// Capabilities reports the embedded store handshake: Core tier only. No raw
// SQL, no percentiles, no cheap high-cardinality filters.
func (s *Store) Capabilities() observe.Capabilities {
	return observe.Capabilities{
		Backend:                "embedded",
		MaxTier:                observe.TierCore,
		RawSQL:                 false,
		Percentiles:            false,
		HighCardinalityFilters: false,
	}
}

// Write appends a batch to the ring, evicting the oldest records if the size
// bound is exceeded. Safe for concurrent callers.
func (s *Store) Write(ctx context.Context, batch []observe.LogRecord) error {
	if len(batch) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range batch {
		r := batch[i]
		if r.Timestamp.IsZero() {
			r.Timestamp = now
		}
		s.buf = append(s.buf, r)
	}
	if over := len(s.buf) - s.maxRecords; over > 0 {
		// Drop oldest.
		s.buf = s.buf[over:]
	}
	return nil
}

// Execute runs a Core-tier query against the in-memory ring. RawSQL is rejected
// (Advanced tier only). Metric queries (Aggregation set) yield a count/rate/
// bytes histogram; everything else yields filtered log rows.
func (s *Store) Execute(ctx context.Context, q *observe.Query) (observe.ResultStream, error) {
	if q.RawSQL != "" {
		return nil, observe.ErrCapabilityUnsupported
	}
	if q.Aggregation != nil && q.Aggregation.Op == observe.AggQuantileOverTime {
		// Percentiles are Advanced tier (ClickHouse only).
		return nil, observe.ErrCapabilityUnsupported
	}

	matchers, err := compileMatchers(q.Selectors)
	if err != nil {
		return nil, err
	}
	filters, err := compileLineFilters(q.LineFilters)
	if err != nil {
		return nil, err
	}

	// Snapshot under the read lock so Execute is consistent and lock-free
	// afterwards.
	s.mu.RLock()
	snapshot := make([]observe.LogRecord, len(s.buf))
	copy(snapshot, s.buf)
	s.mu.RUnlock()

	matched := make([]observe.LogRecord, 0, len(snapshot))
	for _, r := range snapshot {
		if !q.Start.IsZero() && r.Timestamp.Before(q.Start) {
			continue
		}
		if !q.End.IsZero() && !r.Timestamp.Before(q.End) {
			continue // End is exclusive
		}
		if !matchers.matches(r) {
			continue
		}
		if !filters.matches(r.Line) {
			continue
		}
		matched = append(matched, r)
	}

	if q.IsMetricQuery() {
		return newMetricStream(matched, q.Aggregation), nil
	}
	return newRowStream(matched, q), nil
}

// Labels enumerates label names (Selector.Name == "") or the values of a named
// dimension, ranked by occurrence count. Honours Selector.Match and the time
// window.
func (s *Store) Labels(ctx context.Context, sel observe.Selector) ([]observe.LabelValue, error) {
	matchers, err := compileMatchers(sel.Match)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	snapshot := make([]observe.LogRecord, len(s.buf))
	copy(snapshot, s.buf)
	s.mu.RUnlock()

	if sel.Name == "" {
		return s.labelNames(snapshot, matchers, sel), nil
	}
	return s.labelValues(snapshot, matchers, sel), nil
}

func (s *Store) labelNames(snapshot []observe.LogRecord, matchers compiledMatchers, sel observe.Selector) []observe.LabelValue {
	names := map[string]struct{}{}
	for _, r := range snapshot {
		if !inWindow(r, sel.Start, sel.End) || !matchers.matches(r) {
			continue
		}
		for k := range r.StreamLabels() {
			names[k] = struct{}{}
		}
	}
	out := make([]observe.LabelValue, 0, len(names))
	for k := range names {
		out = append(out, observe.LabelValue{Name: k})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return applyLimit(out, sel.Limit)
}

func (s *Store) labelValues(snapshot []observe.LogRecord, matchers compiledMatchers, sel observe.Selector) []observe.LabelValue {
	counts := map[string]int64{}
	for _, r := range snapshot {
		if !inWindow(r, sel.Start, sel.End) || !matchers.matches(r) {
			continue
		}
		if v, ok := recordLabel(r, sel.Name); ok && v != "" {
			counts[v]++
		}
	}
	out := make([]observe.LabelValue, 0, len(counts))
	for v, c := range counts {
		out = append(out, observe.LabelValue{Name: sel.Name, Value: v, Count: c})
	}
	// Rank by count desc, then value asc for stable output.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return applyLimit(out, sel.Limit)
}

// Health is always healthy for the in-process store.
func (s *Store) Health(ctx context.Context) error { return ctx.Err() }

// Close stops the retention sweeper. Safe to call multiple times.
func (s *Store) Close() error {
	s.sweepOnce.Do(func() {
		close(s.stopCh)
		<-s.stopped
	})
	return nil
}

// Len returns the current record count. Useful for metrics and tests.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.buf)
}

func (s *Store) sweepLoop() {
	defer close(s.stopped)
	t := time.NewTicker(defaultSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *Store) sweep() {
	if s.retention < 0 {
		return
	}
	cutoff := s.now().Add(-s.retention)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Records are appended roughly in time order; find the first record at or
	// after the cutoff and drop everything before it.
	idx := sort.Search(len(s.buf), func(i int) bool {
		return !s.buf[i].Timestamp.Before(cutoff)
	})
	if idx > 0 {
		s.buf = append(s.buf[:0], s.buf[idx:]...)
	}
}

func inWindow(r observe.LogRecord, start, end time.Time) bool {
	if !start.IsZero() && r.Timestamp.Before(start) {
		return false
	}
	if !end.IsZero() && !r.Timestamp.Before(end) {
		return false
	}
	return true
}

func applyLimit(in []observe.LabelValue, limit int) []observe.LabelValue {
	if limit > 0 && len(in) > limit {
		return in[:limit]
	}
	return in
}

// recordLabel resolves a dimension name to a value on a record, covering both
// the fixed Rune-native dimensions and custom Labels.
func recordLabel(r observe.LogRecord, name string) (string, bool) {
	switch name {
	case "namespace":
		return r.Namespace, true
	case "service":
		return r.Service, true
	case "instance":
		return r.Instance, true
	case "node":
		return r.Node, true
	case "level":
		return r.Level, true
	case "stream":
		return r.Stream, true
	default:
		v, ok := r.Labels[name]
		return v, ok
	}
}

// --- matcher / filter compilation ---

type compiledMatchers []compiledMatcher

type compiledMatcher struct {
	label string
	op    observe.MatchOp
	value string
	re    *regexp.Regexp
}

func compileMatchers(ms []observe.Matcher) (compiledMatchers, error) {
	out := make(compiledMatchers, 0, len(ms))
	for _, m := range ms {
		cm := compiledMatcher{label: m.Label, op: m.Op, value: m.Value}
		if m.Op == observe.MatchRegex || m.Op == observe.MatchNotRegex {
			re, err := regexp.Compile(m.Value)
			if err != nil {
				return nil, err
			}
			cm.re = re
		}
		out = append(out, cm)
	}
	return out, nil
}

func (cms compiledMatchers) matches(r observe.LogRecord) bool {
	for _, cm := range cms {
		v, _ := recordLabel(r, cm.label)
		var hit bool
		switch cm.op {
		case observe.MatchEqual:
			hit = v == cm.value
		case observe.MatchNotEqual:
			hit = v != cm.value
		case observe.MatchRegex:
			hit = cm.re.MatchString(v)
		case observe.MatchNotRegex:
			hit = !cm.re.MatchString(v)
		}
		if !hit {
			return false
		}
	}
	return true
}

type compiledFilters []compiledFilter

type compiledFilter struct {
	op    observe.LineFilterOp
	value string
	re    *regexp.Regexp
}

func compileLineFilters(fs []observe.LineFilter) (compiledFilters, error) {
	out := make(compiledFilters, 0, len(fs))
	for _, f := range fs {
		cf := compiledFilter{op: f.Op, value: f.Value}
		if f.Op == observe.LineRegex || f.Op == observe.LineNotRegex {
			re, err := regexp.Compile(f.Value)
			if err != nil {
				return nil, err
			}
			cf.re = re
		}
		out = append(out, cf)
	}
	return out, nil
}

func (cfs compiledFilters) matches(line string) bool {
	for _, cf := range cfs {
		var hit bool
		switch cf.op {
		case observe.LineContains:
			hit = strings.Contains(line, cf.value)
		case observe.LineNotContains:
			hit = !strings.Contains(line, cf.value)
		case observe.LineRegex:
			hit = cf.re.MatchString(line)
		case observe.LineNotRegex:
			hit = !cf.re.MatchString(line)
		}
		if !hit {
			return false
		}
	}
	return true
}
