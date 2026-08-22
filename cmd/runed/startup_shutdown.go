package main

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

// closeAll pops every closer in reverse registration order (LIFO), matching
// what deferred calls did. Each closer runs even if an earlier one panics is
// NOT attempted — a panicking Close is a bug we want loud, same as before.
func (c *closerStack) closeAll(logger log.Logger) {
	for i := len(c.entries) - 1; i >= 0; i-- {
		e := c.entries[i]
		logger.Debug("Closing", log.Str("resource", e.name))
		e.close()
	}
}
