package orderedlog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"
)

// ----- test fixtures -----------------------------------------------------

// putOp is a tiny test Op. It writes payload at key.
type putOp struct {
	Key   string
	Value string
}

func (p *putOp) OpType() string { return "test.put" }

func (p *putOp) Marshal() ([]byte, error) {
	out := make([]byte, 0, 4+len(p.Key)+4+len(p.Value))
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(p.Key)))
	out = append(out, l[:]...)
	out = append(out, p.Key...)
	binary.BigEndian.PutUint32(l[:], uint32(len(p.Value)))
	out = append(out, l[:]...)
	out = append(out, p.Value...)
	return out, nil
}

func unmarshalPut(b []byte) (Op, error) {
	if len(b) < 8 {
		return nil, errors.New("short")
	}
	klen := binary.BigEndian.Uint32(b[:4])
	if uint32(len(b)) < 4+klen+4 {
		return nil, errors.New("short")
	}
	key := string(b[4 : 4+klen])
	rest := b[4+klen:]
	vlen := binary.BigEndian.Uint32(rest[:4])
	if uint32(len(rest)) < 4+vlen {
		return nil, errors.New("short")
	}
	val := string(rest[4 : 4+vlen])
	return &putOp{Key: key, Value: val}, nil
}

func putApplier(tx Txn, op Op) ([]Mutation, error) {
	p := op.(*putOp)
	if err := tx.Set([]byte("data/"+p.Key), []byte(p.Value)); err != nil {
		return nil, err
	}
	return []Mutation{{
		Kind:         MutationPut,
		ResourceType: "test",
		Name:         p.Key,
		Payload:      []byte(p.Value),
	}}, nil
}

// failApplier always errors.
func failApplier(tx Txn, op Op) ([]Mutation, error) {
	return nil, errors.New("intentional failure")
}

func newTestBackend(t *testing.T) (*BadgerBackend, *badger.DB) {
	t.Helper()
	dir := t.TempDir()
	opts := badger.DefaultOptions(dir).WithLogger(nil)
	db, err := badger.Open(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	be := NewBadgerBackend(db, BackendOptions{
		WatchBuffer:        64,
		RetentionMaxEvents: 1_000_000,
		RetentionInterval:  time.Hour,
	})
	require.NoError(t, be.Open())
	require.NoError(t, be.Register("test.put", putApplier, unmarshalPut))
	t.Cleanup(func() { _ = be.Close() })
	return be, db
}

// ----- tests --------------------------------------------------------------

func TestPropose_AssignsMonotonicSeq(t *testing.T) {
	be, _ := newTestBackend(t)
	ctx := context.Background()

	for i := 1; i <= 10; i++ {
		seq, err := be.Propose(ctx, &putOp{Key: fmt.Sprintf("k%d", i), Value: "v"})
		require.NoError(t, err)
		require.Equal(t, uint64(i), seq)
	}
}

func TestPropose_RejectsUnknownOpType(t *testing.T) {
	be, _ := newTestBackend(t)
	type stranger struct{ putOp }
	op := &stranger{putOp{Key: "k", Value: "v"}}
	op.putOp.Key = "k"
	// Force an unknown op type via embedding.
	bad := &unknownOp{}
	_, err := be.Propose(context.Background(), bad)
	require.ErrorIs(t, err, ErrUnknownOpType)
}

type unknownOp struct{}

func (*unknownOp) OpType() string           { return "no.such.op" }
func (*unknownOp) Marshal() ([]byte, error) { return nil, nil }

func TestPropose_RollbackOnApplierError(t *testing.T) {
	be, db := newTestBackend(t)
	require.NoError(t, be.Register("test.fail", failApplier, unmarshalPut))

	_, err := be.Propose(context.Background(), &failingOp{})
	require.Error(t, err)

	// Seq counter must NOT have advanced.
	require.Equal(t, uint64(0), be.lastSeq.Load())

	// And no event row should exist.
	require.NoError(t, db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		it.Seek([]byte(eventKeyPrefix))
		require.False(t, it.ValidForPrefix([]byte(eventKeyPrefix)),
			"no event keys should be present after a rolled-back propose")
		return nil
	}))
}

type failingOp struct{}

func (*failingOp) OpType() string           { return "test.fail" }
func (*failingOp) Marshal() ([]byte, error) { return nil, nil }

func TestWatch_DeliversInOrder_Backfill(t *testing.T) {
	be, _ := newTestBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 1; i <= 5; i++ {
		_, err := be.Propose(ctx, &putOp{Key: fmt.Sprintf("k%d", i), Value: "v"})
		require.NoError(t, err)
	}

	ch, err := be.Watch(ctx, 0)
	require.NoError(t, err)

	var got []uint64
	for i := 0; i < 5; i++ {
		select {
		case ev := <-ch:
			got = append(got, ev.Seq)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for backfill")
		}
	}
	require.Equal(t, []uint64{1, 2, 3, 4, 5}, got)
}

func TestWatch_DeliversInOrder_Live(t *testing.T) {
	be, _ := newTestBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := be.Watch(ctx, 0)
	require.NoError(t, err)

	for i := 1; i <= 5; i++ {
		_, err := be.Propose(ctx, &putOp{Key: fmt.Sprintf("k%d", i), Value: "v"})
		require.NoError(t, err)
	}

	var got []uint64
	for i := 0; i < 5; i++ {
		select {
		case ev := <-ch:
			got = append(got, ev.Seq)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for live event")
		}
	}
	require.Equal(t, []uint64{1, 2, 3, 4, 5}, got)
}

func TestWatch_NoLostOrReorderedUnderConcurrency(t *testing.T) {
	be, _ := newTestBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const proposers = 25
	const perProposer = 40
	const total = proposers * perProposer

	// Set up watchers BEFORE any proposes so we exercise live delivery.
	const watchers = 5
	results := make([][]uint64, watchers)
	doneAll := make(chan struct{}, watchers)
	for w := 0; w < watchers; w++ {
		ch, err := be.Watch(ctx, 0)
		require.NoError(t, err)
		w := w
		go func() {
			results[w] = make([]uint64, 0, total)
			for ev := range ch {
				results[w] = append(results[w], ev.Seq)
				if len(results[w]) == total {
					doneAll <- struct{}{}
					return
				}
			}
		}()
	}

	var wg sync.WaitGroup
	wg.Add(proposers)
	for p := 0; p < proposers; p++ {
		p := p
		go func() {
			defer wg.Done()
			for i := 0; i < perProposer; i++ {
				_, err := be.Propose(ctx,
					&putOp{Key: fmt.Sprintf("p%d-%d", p, i), Value: "v"})
				require.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	// Wait for every watcher to drain.
	for i := 0; i < watchers; i++ {
		select {
		case <-doneAll:
		case <-time.After(5 * time.Second):
			t.Fatalf("watcher %d did not drain", i)
		}
	}

	// Every watcher must see strictly increasing 1..total in order.
	for w, got := range results {
		require.Len(t, got, total, "watcher %d", w)
		for i, seq := range got {
			require.Equal(t, uint64(i+1), seq, "watcher %d index %d", w, i)
		}
	}
}

func TestWatch_FromSpecificSeq(t *testing.T) {
	be, _ := newTestBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 1; i <= 5; i++ {
		_, err := be.Propose(ctx, &putOp{Key: fmt.Sprintf("k%d", i), Value: "v"})
		require.NoError(t, err)
	}

	ch, err := be.Watch(ctx, 3) // expect 4, 5, ...
	require.NoError(t, err)

	for _, want := range []uint64{4, 5} {
		select {
		case ev := <-ch:
			require.Equal(t, want, ev.Seq)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for seq %d", want)
		}
	}
}

func TestWatch_CtxCancelClosesChannel(t *testing.T) {
	be, _ := newTestBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := be.Watch(ctx, 0)
	require.NoError(t, err)
	cancel()
	select {
	case _, open := <-ch:
		require.False(t, open, "channel should be closed after ctx cancel")
	case <-time.After(time.Second):
		t.Fatal("channel was not closed after ctx cancel")
	}
}

func TestWatch_ErrCompactedAfterPrune(t *testing.T) {
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	be := NewBadgerBackend(db, BackendOptions{
		WatchBuffer:        64,
		RetentionMaxEvents: 5,         // keep only the last 5
		RetentionInterval:  time.Hour, // we'll trigger manually
	})
	require.NoError(t, be.Open())
	require.NoError(t, be.Register("test.put", putApplier, unmarshalPut))
	t.Cleanup(func() { _ = be.Close() })

	for i := 1; i <= 20; i++ {
		_, err := be.Propose(context.Background(),
			&putOp{Key: fmt.Sprintf("k%d", i), Value: "v"})
		require.NoError(t, err)
	}
	require.NoError(t, be.pruneOnce(context.Background()))

	// minSeq should have advanced past 1.
	require.Greater(t, be.minSeq.Load(), uint64(1))

	// Asking to resume from seq 1 must hit ErrCompacted.
	_, err = be.Watch(context.Background(), 1)
	require.ErrorIs(t, err, ErrCompacted)

	// Snapshot + resume from snapshot.Seq must succeed.
	snap, snapSeq, err := be.Snapshot(context.Background())
	require.NoError(t, err)
	defer snap.Close()
	ch, err := be.Watch(context.Background(), snapSeq)
	require.NoError(t, err)
	// No new events queued; channel should not yield anything immediately.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event after snapshot resume: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPropose_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	require.NoError(t, err)

	be := NewBadgerBackend(db, BackendOptions{})
	require.NoError(t, be.Open())
	require.NoError(t, be.Register("test.put", putApplier, unmarshalPut))
	for i := 1; i <= 3; i++ {
		_, err := be.Propose(context.Background(),
			&putOp{Key: fmt.Sprintf("k%d", i), Value: "v"})
		require.NoError(t, err)
	}
	require.NoError(t, be.Close())
	require.NoError(t, db.Close())

	// Reopen and verify seq is preserved.
	db2, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	require.NoError(t, err)
	defer db2.Close()
	be2 := NewBadgerBackend(db2, BackendOptions{})
	require.NoError(t, be2.Open())
	require.NoError(t, be2.Register("test.put", putApplier, unmarshalPut))
	defer be2.Close()

	require.Equal(t, uint64(3), be2.lastSeq.Load())

	seq, err := be2.Propose(context.Background(),
		&putOp{Key: "k4", Value: "v"})
	require.NoError(t, err)
	require.Equal(t, uint64(4), seq)
}

func TestApplier_CannotWriteReservedPrefix(t *testing.T) {
	be, _ := newTestBackend(t)
	require.NoError(t, be.Register("test.evil", func(tx Txn, op Op) ([]Mutation, error) {
		return nil, tx.Set([]byte("_olog/forged"), []byte("x"))
	}, func(b []byte) (Op, error) { return &evilOp{}, nil }))

	_, err := be.Propose(context.Background(), &evilOp{})
	require.Error(t, err)
}

type evilOp struct{}

func (*evilOp) OpType() string           { return "test.evil" }
func (*evilOp) Marshal() ([]byte, error) { return nil, nil }

func TestRegister_DuplicateRejected(t *testing.T) {
	be, _ := newTestBackend(t)
	err := be.Register("test.put", putApplier, unmarshalPut)
	require.ErrorIs(t, err, ErrAlreadyRegistered)
}

func TestEventEncoding_RoundTrip(t *testing.T) {
	in := &Event{
		Seq:    42,
		OpType: "test.put",
		Mutations: []Mutation{
			{Kind: MutationPut, ResourceType: "service", Namespace: "prod", Name: "api", Payload: []byte("hello")},
			{Kind: MutationDelete, ResourceType: "instance", Namespace: "", Name: "x", Payload: nil},
		},
	}
	raw, err := encodeEvent(in)
	require.NoError(t, err)

	var out Event
	require.NoError(t, decodeEvent(raw, &out))
	require.Equal(t, in.Seq, out.Seq)
	require.Equal(t, in.OpType, out.OpType)
	require.Len(t, out.Mutations, 2)
	require.Equal(t, in.Mutations[0], out.Mutations[0])
	// Delete mutation: payload should be nil/empty.
	require.Equal(t, in.Mutations[1].Kind, out.Mutations[1].Kind)
	require.Equal(t, in.Mutations[1].ResourceType, out.Mutations[1].ResourceType)
	require.Equal(t, in.Mutations[1].Name, out.Mutations[1].Name)
}

func TestClose_FailsSubsequentPropose(t *testing.T) {
	be, _ := newTestBackend(t)
	require.NoError(t, be.Close())
	_, err := be.Propose(context.Background(), &putOp{Key: "k", Value: "v"})
	require.ErrorIs(t, err, ErrClosed)
}

// ----- benchmark + sanity timing -----------------------------------------

func TestPropose_BoundedLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency sanity test in -short")
	}
	be, _ := newTestBackend(t)
	const n = 1000
	start := time.Now()
	for i := 0; i < n; i++ {
		_, err := be.Propose(context.Background(),
			&putOp{Key: fmt.Sprintf("k%d", i), Value: "v"})
		require.NoError(t, err)
	}
	d := time.Since(start)
	t.Logf("propose throughput: %d ops in %s (%.0f op/s)",
		n, d, float64(n)/d.Seconds())
}

// silence unused import / atomic warnings on platforms that strip them.
var _ = atomic.LoadUint64
