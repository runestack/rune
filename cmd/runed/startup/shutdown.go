package startup

import (
	"github.com/runestack/rune/pkg/log"
)

// closerStack is runed's teardown order, made explicit.
//
// It replaces what were five deferred Close calls in main(). The order is
// load-bearing, not tidiness: `olog` and the event recorder are Badger
// backends over the SAME *badger.DB the state store owns, so the OrderedLog
// must close BEFORE the store. Deferred calls encoded that only as LIFO
// source order — invisible unless you read all five defers together and knew
// they shared a handle. Here it is one list, popped in reverse, with the
// reason attached.
//
// Semantics deliberately match the defers they replace: closers run at the
// end of a normal shutdown and are SKIPPED on every os.Exit path (see
// RUNE-313 §4.3 — that is pre-existing behavior, not a choice this refactor
// makes; Badger is left to crash recovery on startup failure).
type closerStack struct {
	entries []closerEntry
}

type closerEntry struct {
	name  string
	close func()
}

// push registers a closer. Call it at the point the resource becomes live,
// exactly where its `defer` used to sit — pushes happen in construction
// order and pops run in reverse.
func (c *closerStack) push(name string, closeFn func()) {
	c.entries = append(c.entries, closerEntry{name: name, close: closeFn})
}

// closeAll runs every closer in reverse registration order (LIFO).
//
// Each is registered as a deferred call rather than invoked in a loop, which
// reproduces the ORIGINAL semantics exactly: with five `defer`s, a panic in one
// closer still ran the remaining ones during unwinding. A plain loop would
// abort on the first panicking closer and strand Badger open.
func (c *closerStack) closeAll(logger log.Logger) {
	for _, e := range c.entries {
		defer func(e closerEntry) {
			logger.Debug("Closing", log.Str("resource", e.name))
			e.close()
		}(e)
	}
}
