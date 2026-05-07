package orderedlog

import (
	"context"

	"github.com/dgraph-io/badger/v4"
)

// Snapshot returns a point-in-time view of committed state and the seq
// it was taken at. The current implementation provides a lightweight
// snapshot that holds a Badger read transaction; richer query helpers
// (Iter, Range, Get-by-prefix) will be added by the watch-stream wiring
// in RUNE-028 once concrete consumers exist.
func (b *BadgerBackend) Snapshot(ctx context.Context) (Snapshot, uint64, error) {
	if b.closed.Load() {
		return nil, 0, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	// Hold proposeMu briefly so the snapshot's seq is consistent with
	// the read txn's view: nothing can commit between "open read txn"
	// and "read seq".
	b.proposeMu.Lock()
	txn := b.db.NewTransaction(false)
	seq := b.lastSeq.Load()
	b.proposeMu.Unlock()
	return &badgerSnapshot{txn: txn}, seq, nil
}

// badgerSnapshot is a read-only view backed by a Badger read txn.
type badgerSnapshot struct {
	txn *badger.Txn
}

func (s *badgerSnapshot) Close() error {
	if s.txn != nil {
		s.txn.Discard()
		s.txn = nil
	}
	return nil
}
