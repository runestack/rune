// Package events provides the persisted, node-local event log that
// powers `rune describe` (Phase 2) and any future telemetry consumer
// (RUNE-0XX) via the cursor view described in RUNE-126 §5.6.
//
// The log is append-only and stored directly on the shared Badger
// instance (no OrderedLog / Raft coupling — events are observability,
// not consensus). Per-key TTL handles GC; an in-memory ring sits in
// front of the store as a read cache + the fold check for repeated
// consecutive events.
//
// Two indexes over one logical record:
//
//	events/<ns>/<kind>/<name>/<seqBE>   → JSON(Event)
//	eventseq/<seqBE>                    → JSON(Event)   (denormalised for cursor reads)
//	eventcursors/<consumerID>           → int64 (last delivered Seq)
//
// Both event indexes carry the same TTL and the same payload; consumer
// cursors are never TTL'd.
package events

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// Default knobs. Override via Options.
const (
	defaultTTL        = time.Hour
	defaultRingPerKey = 50
	cursorKeyPrefix   = "eventcursors/"
	byResourcePrefix  = "events/"
	bySeqPrefix       = "eventseq/"
)

// EventLog is the contract controllers and consumers depend on. It
// matches the surface in RUNE-126 §5.6 verbatim. *Recorder is the
// production implementation; tests may inject fakes.
type EventLog interface {
	Emit(ctx context.Context, e types.Event) error
	ListByResource(ctx context.Context, namespace, kind, name string, limit int) ([]types.Event, error)
	ListSince(ctx context.Context, cursor int64, limit int) ([]types.Event, error)
	ListLatest(ctx context.Context, limit int) ([]types.Event, error)
	ListLatestFiltered(ctx context.Context, ns string, limit, maxScan int) ([]types.Event, error)
	LoadCursor(ctx context.Context, consumerID string) (int64, error)
	SaveCursor(ctx context.Context, consumerID string, seq int64) error
}

// Options configures a Recorder.
type Options struct {
	// TTL is the Badger per-entry TTL applied to event records. Zero
	// uses defaultTTL. Cursors are never TTL'd.
	TTL time.Duration
	// RingPerKey bounds the in-memory ring per (namespace, kind, name).
	// Zero uses defaultRingPerKey.
	RingPerKey int
}

// Recorder is the EventLog implementation. Safe for concurrent use.
type Recorder struct {
	db     *badger.DB
	logger log.Logger
	ttl    time.Duration
	ring   int

	mu      sync.Mutex
	nextSeq int64
	// cache holds the most recent events per (namespace, kind, name).
	// Newest entry is at index len-1. Unbounded number of keys in v1
	// — small per-entry footprint; an LRU eviction is a follow-up if
	// keyspace cardinality ever bites.
	cache map[cacheKey][]types.Event
}

type cacheKey struct{ ns, kind, name string }

// NewRecorder constructs a Recorder over the given Badger handle and
// restores the node-global Seq counter by scanning the by-seq index.
// Pass BadgerStore.DB() — this package never opens its own DB.
func NewRecorder(db *badger.DB, logger log.Logger, opts Options) (*Recorder, error) {
	if db == nil {
		return nil, errors.New("events: nil badger DB")
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	if opts.RingPerKey <= 0 {
		opts.RingPerKey = defaultRingPerKey
	}
	r := &Recorder{
		db:     db,
		logger: logger.WithComponent("events"),
		ttl:    opts.TTL,
		ring:   opts.RingPerKey,
		cache:  make(map[cacheKey][]types.Event),
	}
	if err := r.restoreNextSeq(); err != nil {
		return nil, fmt.Errorf("restore Seq: %w", err)
	}
	return r, nil
}

// restoreNextSeq scans the eventseq/ index for the max existing Seq so
// new emits continue strictly above it across runed restarts.
func (r *Recorder) restoreNextSeq() error {
	var max int64
	err := r.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{
			Prefix:         []byte(bySeqPrefix),
			Reverse:        true,
			PrefetchValues: false,
		})
		defer it.Close()
		// Seek to the largest key under the prefix.
		seekKey := append([]byte(bySeqPrefix), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		it.Seek(seekKey)
		if !it.ValidForPrefix([]byte(bySeqPrefix)) {
			return nil
		}
		k := it.Item().Key()
		if len(k) < len(bySeqPrefix)+8 {
			return nil
		}
		max = decodeSeq(k[len(bySeqPrefix):])
		return nil
	})
	if err != nil {
		return err
	}
	r.nextSeq = max + 1
	return nil
}

// Emit appends an event to the log, folding into the most recent
// like-for-like event for the same (ns, kind, name) when the dedup
// key matches. A folded event keeps its original Seq; only Count and
// LastSeen change. Consumers re-deliver on Count change (idempotent
// via ID).
func (r *Recorder) Emit(ctx context.Context, e types.Event) error {
	if e.Kind == "" || e.Name == "" {
		return errors.New("events: Kind and Name are required")
	}
	now := time.Now().UTC()
	if e.Level == "" {
		e.Level = types.EventLevelInfo
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	ck := cacheKey{e.Namespace, e.Kind, e.Name}
	ring := r.cache[ck]

	// Fold check: identical-to-previous events bump Count + LastSeen
	// on the same Seq instead of appending a new record.
	if n := len(ring); n > 0 && sameFoldKey(ring[n-1], e) {
		prev := ring[n-1]
		prev.Count++
		prev.LastSeen = now
		ring[n-1] = prev
		r.cache[ck] = ring
		return r.persist(prev)
	}

	// New event — assign Seq/ID, set bookkeeping fields.
	e.Seq = r.nextSeq
	r.nextSeq++
	e.ID = fmt.Sprintf("%s/%s/%d", e.Kind, e.Name, e.Seq)
	if e.FirstSeen.IsZero() {
		e.FirstSeen = now
	}
	e.LastSeen = now
	if e.Count == 0 {
		e.Count = 1
	}

	// Append to ring, dropping oldest if over the per-key bound.
	ring = append(ring, e)
	if len(ring) > r.ring {
		ring = ring[len(ring)-r.ring:]
	}
	r.cache[ck] = ring

	return r.persist(e)
}

// persist writes the event under both indexes with the configured TTL.
// Caller must hold r.mu.
func (r *Recorder) persist(e types.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	resKey := resourceKey(e.Namespace, e.Kind, e.Name, e.Seq)
	seqKey := seqKey(e.Seq)
	return r.db.Update(func(txn *badger.Txn) error {
		if err := txn.SetEntry(badger.NewEntry(resKey, payload).WithTTL(r.ttl)); err != nil {
			return err
		}
		return txn.SetEntry(badger.NewEntry(seqKey, payload).WithTTL(r.ttl))
	})
}

// ListByResource returns up to limit most-recent events for the
// resource, newest first. limit <= 0 means "all in the ring/store"
// (capped at RingPerKey).
func (r *Recorder) ListByResource(ctx context.Context, ns, kind, name string, limit int) ([]types.Event, error) {
	if limit <= 0 || limit > r.ring {
		limit = r.ring
	}
	r.mu.Lock()
	ring := r.cache[cacheKey{ns, kind, name}]
	if len(ring) > 0 {
		out := make([]types.Event, 0, min(limit, len(ring)))
		// Newest first.
		for i := len(ring) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, ring[i])
		}
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()

	// Cache miss — scan the by-resource keyspace.
	prefix := []byte(byResourcePrefix + ns + "/" + kind + "/" + name + "/")
	var out []types.Event
	err := r.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{Prefix: prefix, Reverse: true, PrefetchValues: true, PrefetchSize: limit})
		defer it.Close()
		// Reverse iteration needs a seek past the upper bound.
		seekKey := append(append([]byte{}, prefix...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		for it.Seek(seekKey); it.ValidForPrefix(prefix) && len(out) < limit; it.Next() {
			var ev types.Event
			if err := it.Item().Value(func(v []byte) error { return json.Unmarshal(v, &ev) }); err != nil {
				return err
			}
			out = append(out, ev)
		}
		return nil
	})
	return out, err
}

// ListLatestFiltered returns up to limit events in descending Seq order,
// keeping only those in ns (all namespaces when ns is empty), examining at
// most maxScan keys.
//
// The filter runs inside the scan rather than over its result for two
// reasons. The output slice is sized by limit, so a caller's requested
// limit cannot become the allocation — filtering afterwards meant sizing by
// the scan window, which turned a request field into an unbounded make().
// And a quiet namespace is found as long as its events are within maxScan
// keys, where filtering a fixed window returns nothing once busier
// namespaces fill it. maxScan remains a budget: a namespace silent for
// longer than that many events still comes back empty.
func (r *Recorder) ListLatestFiltered(ctx context.Context, ns string, limit, maxScan int) ([]types.Event, error) {
	if limit <= 0 || limit > maxEventScan {
		limit = defaultEventLimit
	}
	if maxScan <= 0 || maxScan > maxEventScan {
		maxScan = maxEventScan
	}
	prefix := []byte(bySeqPrefix)
	out := make([]types.Event, 0, limit)
	err := r.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{Prefix: prefix, Reverse: true, PrefetchValues: true, PrefetchSize: limit})
		defer it.Close()
		// Reverse iteration needs a seek past the upper bound.
		seekKey := append(append([]byte{}, prefix...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		examined := 0
		for it.Seek(seekKey); it.ValidForPrefix(prefix) && len(out) < limit && examined < maxScan; it.Next() {
			examined++
			var ev types.Event
			if err := it.Item().Value(func(v []byte) error { return json.Unmarshal(v, &ev) }); err != nil {
				return err
			}
			if ns != "" && ev.Namespace != ns {
				continue
			}
			out = append(out, ev)
		}
		return nil
	})
	return out, err
}

// Bounds on any caller-influenced event scan. A request field must never
// size an allocation: ListEvents takes its limit from the wire, and an
// int32 there would otherwise reach make() unclamped.
const (
	defaultEventLimit = 100
	maxEventScan      = 10000
)

// ListLatest returns up to limit events in descending Seq order — the
// newest events in the log.
//
// ListSince is the outbox view and iterates forward from a cursor, so
// asking it for "the newest" by passing cursor 0 returns the OLDEST limit
// events instead. A caller that wants recency (the CLI's event views) has
// to scan the sequence index in reverse, which is what this does.
func (r *Recorder) ListLatest(ctx context.Context, limit int) ([]types.Event, error) {
	if limit <= 0 || limit > maxEventScan {
		limit = defaultEventLimit
	}
	prefix := []byte(bySeqPrefix)
	out := make([]types.Event, 0, limit)
	err := r.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{Prefix: prefix, Reverse: true, PrefetchValues: true, PrefetchSize: limit})
		defer it.Close()
		// Reverse iteration needs a seek past the upper bound.
		seekKey := append(append([]byte{}, prefix...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		for it.Seek(seekKey); it.ValidForPrefix(prefix) && len(out) < limit; it.Next() {
			var ev types.Event
			if err := it.Item().Value(func(v []byte) error { return json.Unmarshal(v, &ev) }); err != nil {
				return err
			}
			out = append(out, ev)
		}
		return nil
	})
	return out, err
}

// ListSince returns up to limit events with Seq > cursor, in ascending
// Seq order. No in-tree caller today — it is the outbox contract an
// external consumer tracks with a single cursor; callers wanting recency
// want ListLatest. This is the outbox view RUNE-0XX consumers use; see
// RUNE-126 §5.6.
func (r *Recorder) ListSince(ctx context.Context, cursor int64, limit int) ([]types.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	startSeq := cursor + 1
	if startSeq < 0 {
		startSeq = 0
	}
	seekKey := seqKey(startSeq)
	prefix := []byte(bySeqPrefix)
	out := make([]types.Event, 0, limit)
	err := r.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{Prefix: prefix, PrefetchValues: true, PrefetchSize: limit})
		defer it.Close()
		for it.Seek(seekKey); it.ValidForPrefix(prefix) && len(out) < limit; it.Next() {
			var ev types.Event
			if err := it.Item().Value(func(v []byte) error { return json.Unmarshal(v, &ev) }); err != nil {
				return err
			}
			out = append(out, ev)
		}
		return nil
	})
	return out, err
}

// LoadCursor returns the last-delivered Seq for the consumer, or 0 if
// the consumer is unknown.
func (r *Recorder) LoadCursor(ctx context.Context, consumerID string) (int64, error) {
	if consumerID == "" {
		return 0, errors.New("events: consumerID is required")
	}
	var seq int64
	err := r.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(cursorKeyPrefix + consumerID))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			if len(v) != 8 {
				return fmt.Errorf("cursor %q: bad length %d", consumerID, len(v))
			}
			seq = decodeSeq(v)
			return nil
		})
	})
	return seq, err
}

// SaveCursor records the last-delivered Seq for the consumer. Cursors
// are not TTL'd — they persist until rewritten or explicitly removed.
func (r *Recorder) SaveCursor(ctx context.Context, consumerID string, seq int64) error {
	if consumerID == "" {
		return errors.New("events: consumerID is required")
	}
	if seq < 0 {
		return errors.New("events: cursor seq must be >= 0")
	}
	return r.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(cursorKeyPrefix+consumerID), encodeSeq(seq))
	})
}

// --- helpers -------------------------------------------------------------

func resourceKey(ns, kind, name string, seq int64) []byte {
	// 8-byte big-endian seq suffix makes per-resource scans Seq-ordered.
	prefix := byResourcePrefix + ns + "/" + kind + "/" + name + "/"
	out := make([]byte, 0, len(prefix)+8)
	out = append(out, prefix...)
	out = append(out, encodeSeq(seq)...)
	return out
}

func seqKey(seq int64) []byte {
	out := make([]byte, 0, len(bySeqPrefix)+8)
	out = append(out, bySeqPrefix...)
	out = append(out, encodeSeq(seq)...)
	return out
}

func encodeSeq(s int64) []byte {
	if s < 0 {
		s = 0
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(s)) //nolint:gosec // bounded non-negative above
	return b
}

func decodeSeq(b []byte) int64 {
	if len(b) < 8 {
		return 0
	}
	u := binary.BigEndian.Uint64(b[:8])
	if u > uint64(1)<<63-1 {
		return 0
	}
	return int64(u) //nolint:gosec // bounded above
}

// sameFoldKey compares the dedup-identity fields. UID is included so
// events from a tombstoned incarnation never fold into a fresh one
// reusing the same name.
func sameFoldKey(a, b types.Event) bool {
	return a.UID == b.UID && a.Reason == b.Reason && a.Message == b.Message && a.Kind == b.Kind && a.Name == b.Name
}
