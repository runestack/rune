package types

import "time"

// EventLevel is the severity of a resource Event.
type EventLevel string

const (
	// EventLevelInfo marks a normal lifecycle transition.
	EventLevelInfo EventLevel = "INFO"
	// EventLevelWarn marks a recoverable or retrying condition.
	EventLevelWarn EventLevel = "WARN"
	// EventLevelError marks a failure.
	EventLevelError EventLevel = "ERR"
)

// Event is one entry in a resource's time-ordered diagnostic trail
// (RUNE-126 Phase 2). Controllers emit an Event on every state
// transition and reconcile error; the EventRecorder folds, persists
// and serves them. Events are observability, not consensus state —
// they are node-local and never routed through OrderedLog/Raft.
//
// See RUNE-126 §5.
type Event struct {
	// ID is the stable, per-resource identifier "<kind>/<name>/<resourceSeq>".
	// It is the idempotency key for server-side dedup at any consumer.
	ID string `json:"id" yaml:"id"`

	// Seq is a node-global monotonic sequence number. It lets an
	// external consumer track delivery progress with a single cursor
	// (see EventLog.ListSince) instead of a per-resource map.
	Seq int64 `json:"seq" yaml:"seq"`

	// Namespace / Kind / Name identify the resource the event is about.
	// Kind is "Instance" | "Service" | "Volume" | "Node".
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Kind      string `json:"kind" yaml:"kind"`
	Name      string `json:"name" yaml:"name"`

	// UID ties the event to a specific incarnation of the resource
	// (e.g. one instance UUID), so `describe --previous` can separate
	// a dead incarnation's events from the live one's.
	UID string `json:"uid,omitempty" yaml:"uid,omitempty"`

	// Level is the event severity.
	Level EventLevel `json:"level" yaml:"level"`

	// Reason is the machine-readable slug (e.g. "VolumeNotReady").
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`

	// Message is the human-readable sentence.
	Message string `json:"message" yaml:"message"`

	// FirstSeen / LastSeen bound the fold window: an event identical to
	// the most recent one for its resource (same Reason+Message+UID)
	// bumps LastSeen and Count instead of appending a new record.
	FirstSeen time.Time `json:"firstSeen" yaml:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen" yaml:"lastSeen"`

	// Count is how many times the folded event has occurred (>=1).
	Count int `json:"count" yaml:"count"`
}
