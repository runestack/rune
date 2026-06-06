// Package observe is the native observability storage layer for Rune
// ("RuneSight"). It defines a single LogStore seam between the control-plane
// ingest/query paths and a pluggable backend (an embedded default store, or
// optional ClickHouse / Loki sinks).
//
// Everything in the observability subsystem is built against this package. The
// ObserveService parses a LogQL subset into the Query AST defined here exactly
// once, then hands the AST to a LogStore — no store ever parses LogQL itself.
// Stores declare what they can do via Capabilities, so the dashboard can
// feature-flag advanced widgets per backend.
//
// Ported from the runesight/ scaffold (internal/storage) into Rune core; see
// _docs/plugins/RUNESIGHT_IMPLEMENTATION_PLAN.md §4 (storage abstraction) and
// §6 (what ports from the scaffold).
package observe

import "time"

// LogRecord is a single enriched log line on the ingest path.
//
// The Rune agent forwarder stamps every record with the metadata already on the
// wire (see pkg/types/instance.go): namespace, service, instance, node, and
// arbitrary labels. The shape mirrors rune.api.LogResponse (logs.proto) plus the
// Rune-native identity fields that LogResponse omits.
type LogRecord struct {
	// Timestamp is the event time of the line (not ingest time).
	Timestamp time.Time

	// Line is the raw log content (the "message").
	Line string

	// Stream is the source stream: "stdout" or "stderr". For Outbox-sourced
	// agent/subsystem events this is "event".
	Stream string

	// Level is the parsed severity if known ("info", "warn", "error", ...).
	// Empty when the line was not classified.
	Level string

	// --- Rune-native identity (the indexed/stream dimensions) ---

	// Namespace the producing instance lives in (default: "default").
	Namespace string

	// Service is the parent service name.
	Service string

	// Instance is the instance ID the line came from.
	Instance string

	// Node is the Rune node ID (agent identity) that captured the line.
	Node string

	// Labels are arbitrary key/value stream selectors carried from the
	// instance (pkg/types). High-cardinality labels are only safely
	// queryable on the ClickHouse backend (Advanced tier).
	Labels map[string]string
}

// StreamLabels returns the set of label keys that form the record's stream
// identity for backends that key chunks by stream (e.g. Loki). It is the union
// of the fixed Rune dimensions and any custom Labels keys.
func (r LogRecord) StreamLabels() map[string]string {
	out := make(map[string]string, len(r.Labels)+4)
	for k, v := range r.Labels {
		out[k] = v
	}
	out["namespace"] = r.Namespace
	out["service"] = r.Service
	out["instance"] = r.Instance
	out["node"] = r.Node
	return out
}
