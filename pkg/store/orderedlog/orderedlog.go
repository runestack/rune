// Package orderedlog defines the seam between Rune's control plane and its
// state-mutation backend.
//
// Every state mutation flows through OrderedLog.Propose. Each successful
// Propose is assigned a strictly monotonic sequence number and produces an
// Event that is delivered, in order, to all watchers.
//
// On single-node, the backend is a thin Badger wrapper (see
// BadgerBackend in this package). On multi-node, the backend is a Raft FSM
// implementing the same interface. Consumers above the seam are unchanged
// between phases.
//
// Hard rule: components that touch the protected key prefixes
// ("network/", "endpoints/", "policy/") MUST mutate state via Propose,
// never by writing to Badger directly. A CI lint enforces this.
package orderedlog

import (
	"context"
	"errors"
)

// Sentinel errors returned by OrderedLog implementations.
var (
	// ErrCompacted indicates a watcher requested a sequence number that has
	// already been pruned from the event log. The caller should call
	// Snapshot and resume from the snapshot's sequence number.
	ErrCompacted = errors.New("orderedlog: requested sequence has been compacted")

	// ErrUnknownOpType indicates Propose was called with an Op whose OpType
	// has no registered Applier.
	ErrUnknownOpType = errors.New("orderedlog: unknown op type")

	// ErrAlreadyRegistered indicates Register was called with an OpType that
	// already has an Applier bound.
	ErrAlreadyRegistered = errors.New("orderedlog: op type already registered")

	// ErrClosed indicates the OrderedLog has been closed.
	ErrClosed = errors.New("orderedlog: closed")
)

// MutationKind classifies a single resource change carried by an Event.
type MutationKind uint8

const (
	// MutationPut indicates the resource was created or updated.
	MutationPut MutationKind = iota + 1
	// MutationDelete indicates the resource was deleted.
	MutationDelete
)

// Op is a serializable description of a state mutation.
//
// Consumers produce Ops; backends own the application. The single-node
// backend applies Ops inside a Badger transaction. The multi-node backend
// will serialize Ops, ship them to followers, and replay each one on every
// peer. An Op therefore must not carry references to in-process state
// (e.g. *badger.Txn or function-shaped fields).
type Op interface {
	// OpType returns a stable identifier used by the codec registry.
	// Stable across releases; treat as a wire identifier.
	OpType() string

	// Marshal returns the canonical byte representation of the Op.
	// Two equivalent Ops MUST marshal to byte-equal output.
	Marshal() ([]byte, error)
}

// OpUnmarshaler reconstructs an Op of a given type from bytes. One is
// registered alongside each Applier so the backend can decode persisted
// or replicated Ops.
type OpUnmarshaler func(data []byte) (Op, error)

// Mutation describes a single committed change carried by an Event.
//
// One Op may produce multiple Mutations (for example, allocating a VIP
// records a free-list entry and a service-to-VIP binding in one shot).
type Mutation struct {
	// Kind classifies the change.
	Kind MutationKind
	// ResourceType is a short, stable identifier (e.g. "service",
	// "endpoints", "clusternetwork"). Used by watchers to filter.
	ResourceType string
	// Namespace scopes the mutation. Empty for cluster-scoped resources.
	Namespace string
	// Name uniquely identifies the resource within (ResourceType, Namespace).
	Name string
	// Payload is the marshalled new value for Put mutations and nil for
	// Delete mutations. Format is the resource type's chosen encoding;
	// consumers and the backend must agree on it out of band.
	Payload []byte
}

// Event is a single committed entry in the ordered log. Events are
// delivered to watchers strictly in Seq order.
type Event struct {
	// Seq is the strictly monotonic sequence assigned by the backend.
	// Starts at 1; never zero, never reused.
	Seq uint64
	// OpType is the Op type that produced this Event.
	OpType string
	// Mutations are the resource changes the Op committed.
	Mutations []Mutation
}

// Txn is the narrow interface a backend hands to an Applier. It wraps
// the backend's underlying transaction (a *badger.Txn on single-node;
// a Raft FSM apply step on multi-node) but never exposes it. Appliers
// MUST NOT type-assert through this interface to the underlying type.
type Txn interface {
	// Get returns the value stored at key, or os.ErrNotExist (wrapped)
	// if the key has never been written.
	Get(key []byte) ([]byte, error)
	// Set writes value at key, creating the key if needed.
	Set(key, value []byte) error
	// Delete removes the key. Idempotent.
	Delete(key []byte) error
}

// Applier applies a single Op to backend state inside a backend-owned
// transaction and returns the Mutations the Op committed.
//
// Appliers MUST be deterministic: for the same (prior state, Op) input
// they MUST produce the same Mutations. This is what makes the Raft
// backend swap safe in Phase 2.
//
// An Applier MUST NOT capture or persist the supplied Txn beyond the
// call. The backend invalidates it on return.
type Applier func(tx Txn, op Op) ([]Mutation, error)

// Snapshot is an opaque, point-in-time view of committed state.
//
// Backends return a Snapshot together with the sequence number it was
// taken at. A watcher that received ErrCompacted should call Snapshot,
// rebuild its local view from it, and resume Watch from snapshot.Seq+1.
//
// Range iterates committed key/value pairs whose keys begin with the
// given prefix in lexicographic order. The visitor's slices are valid
// only for the duration of the call (Badger's Item.Value semantics);
// callers must copy if they need to retain them. A non-nil error from
// the visitor terminates iteration and is returned.
type Snapshot interface {
	// Close releases any resources held by the snapshot. Safe to call
	// multiple times.
	Close() error

	// Range iterates committed key/value pairs sharing prefix and
	// invokes visit for each. visit must not retain the byte slices
	// past its return. A non-nil error from visit aborts iteration
	// and is returned to the caller.
	Range(prefix []byte, visit func(key, value []byte) error) error
}

// OrderedLog is the seam.
type OrderedLog interface {
	// Register binds an Applier and OpUnmarshaler to an OpType. It MUST
	// be called for every OpType the consumer intends to Propose, before
	// the first Propose for that type. Returns ErrAlreadyRegistered if
	// called twice for the same type.
	Register(opType string, applier Applier, unmarshal OpUnmarshaler) error

	// Propose serializes the Op, applies it via the registered Applier
	// inside a backend transaction, assigns a sequence number, persists
	// the resulting Event, and publishes it to watchers.
	//
	// Propose returns the assigned sequence number on success. On error,
	// the transaction is rolled back and no sequence number is consumed.
	//
	// Propose is strictly serialized: at most one Op is in-flight per
	// OrderedLog at any time.
	Propose(ctx context.Context, op Op) (seq uint64, err error)

	// Watch subscribes to the event stream starting at fromSeq+1.
	//
	// Pass fromSeq=0 to receive every event from the start of the
	// retained log. If fromSeq is older than the retention window,
	// Watch returns ErrCompacted; the caller should Snapshot and resume.
	//
	// The returned channel is closed when ctx is cancelled or the
	// OrderedLog is closed. Slow consumers may be dropped (channel
	// closed early) if they fall too far behind; the buffer size is
	// implementation-defined.
	Watch(ctx context.Context, fromSeq uint64) (<-chan Event, error)

	// Snapshot returns a point-in-time view of committed state and the
	// sequence number it was taken at. Use after ErrCompacted.
	Snapshot(ctx context.Context) (snap Snapshot, seq uint64, err error)

	// Close shuts the OrderedLog down. In-flight Propose calls return
	// ErrClosed; existing watchers' channels are closed.
	Close() error
}
