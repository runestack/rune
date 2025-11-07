package log

import (
	"strings"
	"sync"

	"github.com/runestack/rune/pkg/utils"
)

// RedactionHook redacts sensitive values from log entries.
type RedactionHook struct {
	fields []string
}

// Levels returns the levels this hook should be called for.
func (h *RedactionHook) Levels() []Level {
	return []Level{DebugLevel, InfoLevel, WarnLevel, ErrorLevel, FatalLevel}
}

// Fire executes the hook's logic for a log entry.
func (h *RedactionHook) Fire(entry *Entry) error {
	// Iterate through fields that should be redacted
	for _, field := range h.fields {
		// Check if the field exists in the entry
		if value, ok := entry.Fields[field]; ok {
			// Redact the value based on its type
			switch value.(type) {
			case string:
				entry.Fields[field] = "[REDACTED]"
			default:
				entry.Fields[field] = "[REDACTED]"
			}
		}
	}
	return nil
}

// NewRedactionHook creates a new redaction hook.
func NewRedactionHook(fields []string) *RedactionHook {
	return &RedactionHook{
		fields: fields,
	}
}

// SamplingHook implements sampling for high-volume logs.
type SamplingHook struct {
	mu         sync.Mutex
	counters   map[string]uint64
	initial    uint64
	thereafter uint64
}

// Levels returns the levels this hook should be called for.
func (h *SamplingHook) Levels() []Level {
	return []Level{DebugLevel, InfoLevel, WarnLevel, ErrorLevel, FatalLevel}
}

// Fire executes the hook's logic for a log entry.
func (h *SamplingHook) Fire(entry *Entry) error {
	// Create a key based on level and message to track similar entries
	key := strings.Join([]string{entry.Level.String(), entry.Message}, ":")

	h.mu.Lock()
	counter, ok := h.counters[key]
	if !ok {
		counter = 0
		h.counters[key] = 0
	}

	// Check if the entry should be sampled out
	sample := counter < h.initial || (counter-h.initial)%h.thereafter == 0

	// Update counter
	h.counters[key]++
	h.mu.Unlock()

	if !sample {
		// Add a field to indicate sampling if not already present
		if _, ok := entry.Fields["_sampled"]; !ok {
			entry.Fields["_sampled"] = true
		}

		// Signal that this entry should be dropped
		return ErrEntrySampled
	}

	return nil
}

// NewSamplingHook creates a new sampling hook.
func NewSamplingHook(initial, thereafter int) *SamplingHook {
	if initial < 0 {
		initial = 0
	}
	if thereafter <= 0 {
		thereafter = 1
	}

	return &SamplingHook{
		counters:   make(map[string]uint64),
		initial:    utils.ToUint64NonNegative(initial),
		thereafter: utils.ToUint64NonNegative(thereafter),
	}
}

// ErrEntrySampled is returned when an entry is sampled out.
var ErrEntrySampled = &entrySampledError{}

type entrySampledError struct{}

func (e *entrySampledError) Error() string {
	return "entry sampled"
}

// OTELHook integrates with OpenTelemetry to add trace information to logs.
type OTELHook struct{}

// Levels returns the levels this hook should be called for.
func (h *OTELHook) Levels() []Level {
	return []Level{DebugLevel, InfoLevel, WarnLevel, ErrorLevel, FatalLevel}
}

// Fire executes the hook's logic for a log entry.
func (h *OTELHook) Fire(entry *Entry) error {
	// This is a placeholder for OTEL integration
	// When implemented, it would extract trace/span IDs from context
	// and add them to the log entry

	// For now, do nothing
	return nil
}

// NewOTELHook creates a new OpenTelemetry hook.
func NewOTELHook() *OTELHook {
	return &OTELHook{}
}
