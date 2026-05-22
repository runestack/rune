package orderedlog

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
)

// Key layout (all keys live under the "_olog/" prefix; this prefix is
// reserved by orderedlog and MUST NOT be touched by any other package):
//
//	_olog/seq                       -> uint64 big-endian, last assigned seq
//	_olog/event/<seq:big-endian>    -> marshalled storedEvent
//
// Big-endian encoding keeps event keys in lexicographic == sequence order,
// so a Badger range scan yields events in delivery order.

const (
	// keyPrefix is reserved by orderedlog. All orderedlog keys live here.
	// Any other package writing under this prefix is a bug.
	keyPrefix = "_olog/"

	seqKey         = keyPrefix + "seq"
	eventKeyPrefix = keyPrefix + "event/"

	// defaultWatchBuffer caps the number of events buffered per watcher
	// before the backend drops the watcher.
	defaultWatchBuffer = 1024
)

// BackendOptions tunes the BadgerBackend.
type BackendOptions struct {
	// WatchBuffer is the per-watcher channel capacity. Defaults to 1024.
	// A watcher that fails to drain within capacity is dropped (its
	// channel is closed) so a single slow consumer cannot stall others.
	WatchBuffer int

	// RetentionAge is how long committed events are kept in the log. A
	// watcher requesting a sequence older than this window receives
	// ErrCompacted. Defaults to 24h. Set to 0 to disable age-based GC.
	RetentionAge time.Duration

	// RetentionMaxEvents is the hard cap on the number of events kept in
	// the log. The pruner keeps at least this many, regardless of age.
	// Defaults to 100_000. Set to 0 to disable count-based GC.
	RetentionMaxEvents uint64

	// RetentionInterval is how often the background pruner runs.
	// Defaults to 5 minutes.
	RetentionInterval time.Duration

	// Logger is the structured logger to use. Defaults to the global
	// "orderedlog" logger.
	Logger log.Logger
}

func (o *BackendOptions) defaults() {
	if o.WatchBuffer <= 0 {
		o.WatchBuffer = defaultWatchBuffer
	}
	if o.RetentionAge == 0 {
		o.RetentionAge = 24 * time.Hour
	}
	if o.RetentionMaxEvents == 0 {
		o.RetentionMaxEvents = 100_000
	}
	if o.RetentionInterval == 0 {
		o.RetentionInterval = 5 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = log.GetDefaultLogger().WithComponent("orderedlog")
	}
}

// BadgerBackend is the single-node implementation of OrderedLog. It
// shares a *badger.DB with the rest of the control plane and owns the
// "_olog/" key prefix exclusively.
type BadgerBackend struct {
	db   *badger.DB
	opts BackendOptions
	log  log.Logger

	// proposeMu serializes Propose calls. The single-node backend does
	// not need higher concurrency at Rune's target scale, and serial
	// application gives strict total ordering without conflict retries.
	// The Raft backend will replace this with the Raft apply loop.
	proposeMu sync.Mutex

	// lastSeq is the highest assigned sequence number. Loaded from
	// _olog/seq at Open and bumped under proposeMu.
	lastSeq atomic.Uint64

	// minSeq is the smallest sequence still retained in the log. The
	// pruner advances it. Watchers compare fromSeq against this to
	// decide whether to return ErrCompacted.
	minSeq atomic.Uint64

	registryMu sync.RWMutex
	appliers   map[string]Applier
	decoders   map[string]OpUnmarshaler

	subsMu sync.Mutex
	subs   map[*subscription]struct{}

	// closed is set by Close. Subsequent Propose / Watch calls fail.
	closed atomic.Bool
	// done is closed by Close so background goroutines can exit.
	done chan struct{}
	// wg tracks background goroutines started by Open.
	wg sync.WaitGroup
}

// subscription is a single Watch.
type subscription struct {
	ch     chan Event
	from   uint64 // last delivered seq; next event must be from+1
	cancel context.CancelFunc
}

// NewBadgerBackend constructs a BadgerBackend bound to db. Call Open
// before Propose / Watch.
func NewBadgerBackend(db *badger.DB, opts BackendOptions) *BadgerBackend {
	opts.defaults()
	return &BadgerBackend{
		db:       db,
		opts:     opts,
		log:      opts.Logger,
		appliers: make(map[string]Applier),
		decoders: make(map[string]OpUnmarshaler),
		subs:     make(map[*subscription]struct{}),
		done:     make(chan struct{}),
	}
}

// Open loads the persisted sequence counter and starts background
// retention. It is safe to call multiple times only if the previous
// Open was followed by Close.
func (b *BadgerBackend) Open() error {
	if b.db == nil {
		return errors.New("orderedlog: nil badger db")
	}
	// Load last seq.
	var last uint64
	err := b.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(seqKey))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			if len(v) != 8 {
				return fmt.Errorf("orderedlog: corrupt seq key (len=%d)", len(v))
			}
			last = binary.BigEndian.Uint64(v)
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("orderedlog: load seq: %w", err)
	}
	b.lastSeq.Store(last)

	// Discover the smallest retained event seq by scanning the first key
	// under the event prefix. If the log is empty, minSeq stays 0.
	if err := b.refreshMinSeq(); err != nil {
		return fmt.Errorf("orderedlog: scan min seq: %w", err)
	}

	b.closed.Store(false)
	b.wg.Add(1)
	go b.retentionLoop()

	b.log.Info("orderedlog opened",
		log.F("last_seq", last),
		log.F("min_seq", b.minSeq.Load()),
	)
	return nil
}

// Close stops background work and closes all watcher channels. It does
// NOT close the underlying *badger.DB; the caller owns that.
func (b *BadgerBackend) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(b.done)
	// Cancel all subscribers so their goroutines return.
	b.subsMu.Lock()
	for s := range b.subs {
		s.cancel()
	}
	b.subs = nil
	b.subsMu.Unlock()
	b.wg.Wait()
	return nil
}

// Register binds an Applier and OpUnmarshaler to an OpType.
func (b *BadgerBackend) Register(opType string, applier Applier, unmarshal OpUnmarshaler) error {
	if opType == "" {
		return errors.New("orderedlog: empty op type")
	}
	if applier == nil || unmarshal == nil {
		return errors.New("orderedlog: nil applier or unmarshaler")
	}
	b.registryMu.Lock()
	defer b.registryMu.Unlock()
	if _, ok := b.appliers[opType]; ok {
		return fmt.Errorf("%w: %q", ErrAlreadyRegistered, opType)
	}
	b.appliers[opType] = applier
	b.decoders[opType] = unmarshal
	return nil
}

// Propose serializes op, applies it inside a Badger txn, persists the
// resulting Event, and publishes it.
func (b *BadgerBackend) Propose(ctx context.Context, op Op) (uint64, error) {
	if b.closed.Load() {
		return 0, ErrClosed
	}
	if op == nil {
		return 0, errors.New("orderedlog: nil op")
	}
	opType := op.OpType()
	b.registryMu.RLock()
	applier, ok := b.appliers[opType]
	b.registryMu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownOpType, opType)
	}

	// Pre-marshal the op so an applier panic / error doesn't leave us
	// guessing about what the wire form would have been; also exercises
	// the Marshal contract on every commit.
	if _, err := op.Marshal(); err != nil {
		return 0, fmt.Errorf("orderedlog: op marshal: %w", err)
	}

	// Honor ctx cancellation prior to taking the global serializer.
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	b.proposeMu.Lock()
	defer b.proposeMu.Unlock()

	if b.closed.Load() {
		return 0, ErrClosed
	}

	nextSeq := b.lastSeq.Load() + 1

	var (
		mutations []Mutation
		applyErr  error
	)
	err := b.db.Update(func(txn *badger.Txn) error {
		wrapped := &badgerTxn{txn: txn}
		mutations, applyErr = applier(wrapped, op)
		if applyErr != nil {
			return applyErr
		}
		// Persist the event.
		ev := Event{Seq: nextSeq, OpType: opType, Mutations: mutations}
		raw, err := encodeEvent(&ev)
		if err != nil {
			return fmt.Errorf("orderedlog: encode event: %w", err)
		}
		if err := txn.Set(eventKey(nextSeq), raw); err != nil {
			return err
		}
		// Bump the persisted seq counter.
		var seqBuf [8]byte
		binary.BigEndian.PutUint64(seqBuf[:], nextSeq)
		return txn.Set([]byte(seqKey), seqBuf[:])
	})
	if err != nil {
		return 0, err
	}

	// Commit succeeded; bump in-memory seq and publish.
	b.lastSeq.Store(nextSeq)
	// First successful commit also defines minSeq if the log was empty.
	b.minSeq.CompareAndSwap(0, nextSeq)

	b.publish(Event{Seq: nextSeq, OpType: opType, Mutations: mutations})
	return nextSeq, nil
}

// Watch subscribes to the event stream from fromSeq+1.
func (b *BadgerBackend) Watch(ctx context.Context, fromSeq uint64) (<-chan Event, error) {
	if b.closed.Load() {
		return nil, ErrClosed
	}
	min := b.minSeq.Load()
	// fromSeq+1 is the first event we'd deliver. If that's older than
	// what we still retain, the caller must Snapshot.
	// Special case: empty log (min == 0) and fromSeq == 0 is fine — we
	// just deliver everything that comes next.
	if min > 0 && fromSeq+1 < min {
		return nil, ErrCompacted
	}

	subCtx, cancel := context.WithCancel(ctx)
	sub := &subscription{
		ch:     make(chan Event, b.opts.WatchBuffer),
		from:   fromSeq,
		cancel: cancel,
	}

	// Backfill any already-committed events between fromSeq+1 and the
	// current head, then register the live subscription. We hold
	// proposeMu briefly to make the cutover atomic: no event can be
	// committed between "scan up to head" and "register live", so we
	// neither lose nor duplicate.
	b.proposeMu.Lock()
	if err := b.backfill(sub); err != nil {
		b.proposeMu.Unlock()
		cancel()
		return nil, err
	}
	b.subsMu.Lock()
	if b.subs == nil {
		// Closed between the closed.Load() check and here.
		b.subsMu.Unlock()
		b.proposeMu.Unlock()
		cancel()
		close(sub.ch)
		return nil, ErrClosed
	}
	b.subs[sub] = struct{}{}
	b.subsMu.Unlock()
	b.proposeMu.Unlock()

	// Goroutine that closes the channel and detaches the subscription
	// when ctx is cancelled or the backend closes.
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		select {
		case <-subCtx.Done():
		case <-b.done:
		}
		b.removeSub(sub)
	}()
	return sub.ch, nil
}

func (b *BadgerBackend) removeSub(s *subscription) {
	b.subsMu.Lock()
	defer b.subsMu.Unlock()
	if b.subs != nil {
		delete(b.subs, s)
	}
	// Always close the channel — including when b.subs has already
	// been nilled by a concurrent Close(). Each subscription has
	// exactly one goroutine that calls removeSub exactly once, so
	// this never double-closes. The old behaviour skipped the close
	// when b.subs == nil, which left every Watch consumer ranging
	// over a channel that never closed — they hung forever on
	// backend shutdown instead of seeing the watch end and reacting.
	close(s.ch)
}

// backfill drains events strictly greater than sub.from up to the
// current head into sub.ch. Caller MUST hold b.proposeMu.
func (b *BadgerBackend) backfill(sub *subscription) error {
	head := b.lastSeq.Load()
	if sub.from >= head {
		return nil
	}
	return b.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		start := eventKey(sub.from + 1)
		for it.Seek(start); it.ValidForPrefix([]byte(eventKeyPrefix)); it.Next() {
			item := it.Item()
			var ev Event
			if err := item.Value(func(v []byte) error {
				return decodeEvent(v, &ev)
			}); err != nil {
				return fmt.Errorf("orderedlog: decode backfill event: %w", err)
			}
			if !b.deliver(sub, ev) {
				// Subscriber buffer full during backfill; treat as
				// slow consumer and drop. Caller will see a closed
				// channel and must reconnect from a snapshot.
				return nil
			}
			sub.from = ev.Seq
		}
		return nil
	})
}

// publish fans an event out to all live subscriptions. Slow consumers
// are dropped (channel closed) rather than blocking the propose path.
func (b *BadgerBackend) publish(ev Event) {
	b.subsMu.Lock()
	if b.subs == nil {
		b.subsMu.Unlock()
		return
	}
	// Snapshot subscriber list so we don't hold the lock during sends.
	subs := make([]*subscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.subsMu.Unlock()

	for _, s := range subs {
		if !b.deliver(s, ev) {
			b.removeSub(s)
		}
	}
}

// deliver attempts a non-blocking send to s. Returns false if the
// channel is full.
func (b *BadgerBackend) deliver(s *subscription, ev Event) bool {
	select {
	case s.ch <- ev:
		s.from = ev.Seq
		return true
	default:
		return false
	}
}

// refreshMinSeq scans the first event key in the log and updates minSeq.
func (b *BadgerBackend) refreshMinSeq() error {
	return b.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		prefix := []byte(eventKeyPrefix)
		it.Seek(prefix)
		if !it.ValidForPrefix(prefix) {
			b.minSeq.Store(0)
			return nil
		}
		key := it.Item().Key()
		if len(key) != len(eventKeyPrefix)+8 {
			return fmt.Errorf("orderedlog: corrupt event key length=%d", len(key))
		}
		seq := binary.BigEndian.Uint64(key[len(eventKeyPrefix):])
		b.minSeq.Store(seq)
		return nil
	})
}

// badgerTxn adapts *badger.Txn to the narrow Txn interface. The
// underlying txn is intentionally unexported so consumers cannot
// type-assert their way around the seam.
type badgerTxn struct {
	txn *badger.Txn
}

func (t *badgerTxn) Get(key []byte) ([]byte, error) {
	item, err := t.txn.Get(key)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, string(key))
	}
	if err != nil {
		return nil, err
	}
	return item.ValueCopy(nil)
}

func (t *badgerTxn) Set(key, value []byte) error {
	// Defensive: forbid an applier from writing into orderedlog's
	// reserved prefix. This is a programming-error guard, not security.
	if bytes.HasPrefix(key, []byte(keyPrefix)) {
		return fmt.Errorf("orderedlog: applier wrote reserved key %q", string(key))
	}
	return t.txn.Set(key, value)
}

func (t *badgerTxn) Delete(key []byte) error {
	if bytes.HasPrefix(key, []byte(keyPrefix)) {
		return fmt.Errorf("orderedlog: applier deleted reserved key %q", string(key))
	}
	return t.txn.Delete(key)
}

// eventKey returns the Badger key for a given sequence number.
func eventKey(seq uint64) []byte {
	out := make([]byte, len(eventKeyPrefix)+8)
	copy(out, eventKeyPrefix)
	binary.BigEndian.PutUint64(out[len(eventKeyPrefix):], seq)
	return out
}

// encodeEvent / decodeEvent: simple length-prefixed framing. Layout:
//
//	uint64 seq | uint16 opTypeLen | opType | uint16 nMutations
//	for each mutation:
//	    uint8 kind | uint16 rtypeLen | rtype |
//	    uint16 nsLen | ns | uint16 nameLen | name |
//	    uint32 payloadLen | payload
//
// This is intentionally trivial; we'll swap to protobuf in RUNE-028
// when the watch stream goes on the wire. For RUNE-039 it's enough to
// validate round-trips.

func encodeEvent(ev *Event) ([]byte, error) {
	var buf bytes.Buffer
	if len(ev.OpType) > 0xFFFF {
		return nil, fmt.Errorf("orderedlog: op type too long")
	}
	if len(ev.Mutations) > 0xFFFF {
		return nil, fmt.Errorf("orderedlog: too many mutations")
	}
	var hdr [8]byte
	binary.BigEndian.PutUint64(hdr[:], ev.Seq)
	buf.Write(hdr[:])
	if err := writeShortString(&buf, ev.OpType); err != nil {
		return nil, err
	}
	var nMut [2]byte
	binary.BigEndian.PutUint16(nMut[:], uint16(len(ev.Mutations))) //nolint:gosec // G115: orderedlog rejects events with >65535 mutations upstream
	buf.Write(nMut[:])
	for i := range ev.Mutations {
		m := &ev.Mutations[i]
		if err := buf.WriteByte(byte(m.Kind)); err != nil {
			return nil, err
		}
		if err := writeShortString(&buf, m.ResourceType); err != nil {
			return nil, err
		}
		if err := writeShortString(&buf, m.Namespace); err != nil {
			return nil, err
		}
		if err := writeShortString(&buf, m.Name); err != nil {
			return nil, err
		}
		var pl [4]byte
		binary.BigEndian.PutUint32(pl[:], uint32(len(m.Payload))) //nolint:gosec // G115: payloads are bounded by orderedlog max-payload-size (<<4GiB)
		buf.Write(pl[:])
		buf.Write(m.Payload)
	}
	return buf.Bytes(), nil
}

func decodeEvent(data []byte, out *Event) error {
	r := bytes.NewReader(data)
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	out.Seq = binary.BigEndian.Uint64(hdr[:])
	op, err := readShortString(r)
	if err != nil {
		return err
	}
	out.OpType = op
	var nMutBuf [2]byte
	if _, err := io.ReadFull(r, nMutBuf[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint16(nMutBuf[:])
	out.Mutations = make([]Mutation, n)
	for i := range out.Mutations {
		kindByte, err := r.ReadByte()
		if err != nil {
			return err
		}
		out.Mutations[i].Kind = MutationKind(kindByte)
		if out.Mutations[i].ResourceType, err = readShortString(r); err != nil {
			return err
		}
		if out.Mutations[i].Namespace, err = readShortString(r); err != nil {
			return err
		}
		if out.Mutations[i].Name, err = readShortString(r); err != nil {
			return err
		}
		var pl [4]byte
		if _, err := io.ReadFull(r, pl[:]); err != nil {
			return err
		}
		plen := binary.BigEndian.Uint32(pl[:])
		if plen > 0 {
			out.Mutations[i].Payload = make([]byte, plen)
			if _, err := io.ReadFull(r, out.Mutations[i].Payload); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeShortString(buf *bytes.Buffer, s string) error {
	if len(s) > 0xFFFF {
		return fmt.Errorf("orderedlog: string too long (%d)", len(s))
	}
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(s))) //nolint:gosec // G115: writeShortString rejects len>0xFFFF on the line above
	buf.Write(l[:])
	buf.WriteString(s)
	return nil
}

func readShortString(r *bytes.Reader) (string, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint16(l[:])
	if n == 0 {
		return "", nil
	}
	out := make([]byte, n)
	if _, err := io.ReadFull(r, out); err != nil {
		return "", err
	}
	return string(out), nil
}
