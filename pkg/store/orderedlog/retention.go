package orderedlog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
)

// retentionLoop runs in a goroutine started by Open. It periodically
// prunes events older than RetentionAge and beyond RetentionMaxEvents,
// whichever leaves more behind.
func (b *BadgerBackend) retentionLoop() {
	defer b.wg.Done()
	t := time.NewTicker(b.opts.RetentionInterval)
	defer t.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-t.C:
			if err := b.pruneOnce(context.Background()); err != nil {
				b.log.Warn("orderedlog retention error", log.Err(err))
			}
		}
	}
}

// pruneOnce computes the smallest seq we must keep and deletes
// everything strictly below it. The watermark is the larger of:
//
//   - the seq corresponding to "head - RetentionMaxEvents + 1"
//     (count-based floor; keep at least RetentionMaxEvents)
//   - the smallest seq whose committed-at time is within RetentionAge
//
// Because events do not currently carry a per-event timestamp on disk
// (we keep the framing minimal in RUNE-039), age-based pruning falls
// back to "keep all retained events" in this iteration. Count-based
// pruning is the active mechanism. Age-based pruning will become
// effective when the watch-stream wire format lands in RUNE-028 and we
// can carry a timestamp without re-encoding the entire log.
//
// This is a deliberate trade-off: the count cap is the safety net that
// matters operationally (it bounds disk use); the age cap is a nice-to-
// have we'll wire up cheaply once the format already carries a time.
func (b *BadgerBackend) pruneOnce(ctx context.Context) error {
	head := b.lastSeq.Load()
	if head == 0 {
		return nil
	}
	keep := b.opts.RetentionMaxEvents
	var floor uint64
	if keep > 0 && head > keep {
		floor = head - keep + 1
	} else {
		// Keep everything; nothing to do.
		return nil
	}
	// Walk event keys < floor and delete in batches.
	const batchSize = 1024
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var deleted int
		err := b.db.Update(func(txn *badger.Txn) error {
			opts := badger.DefaultIteratorOptions
			opts.PrefetchValues = false
			it := txn.NewIterator(opts)
			defer it.Close()
			prefix := []byte(eventKeyPrefix)
			for it.Seek(prefix); it.ValidForPrefix(prefix) && deleted < batchSize; it.Next() {
				key := it.Item().KeyCopy(nil)
				if len(key) != len(eventKeyPrefix)+8 {
					return fmt.Errorf("orderedlog: corrupt event key during prune")
				}
				seq := binary.BigEndian.Uint64(key[len(eventKeyPrefix):])
				if seq >= floor {
					break
				}
				if err := txn.Delete(key); err != nil {
					return err
				}
				deleted++
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, badger.ErrTxnTooBig) {
				// Retry with a fresh txn; previous deletes have committed.
				continue
			}
			return err
		}
		if deleted < batchSize {
			break
		}
	}
	if err := b.refreshMinSeq(); err != nil {
		return err
	}
	b.log.Debug("orderedlog pruned",
		log.F("head", head),
		log.F("floor", floor),
		log.F("min_seq", b.minSeq.Load()),
	)
	return nil
}
