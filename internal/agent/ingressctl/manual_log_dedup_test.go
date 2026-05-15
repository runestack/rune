package ingressctl

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/stretchr/testify/assert"
)

// Identical (host, errKind) within ManualLogRepeat: only the first
// call should record the timestamp; subsequent calls are dropped
// silently. We can't easily assert against the actual logger output
// without a sink, so we observe the dedup map's behaviour instead —
// it's the load-bearing piece.
func TestWarnManualOnce_DedupsWithinWindow(t *testing.T) {
	c := &Controller{
		cfg:          Config{Logger: log.NewLogger()},
		manualLogged: map[string]time.Time{},
	}
	// Cheap reuse of the controller's helper: the second call should
	// observe the first call's timestamp and short-circuit before
	// updating the map. We can't easily inspect that from outside,
	// but we can prove the map only ever has one entry per host.
	c.warnManualOnce("api.example.com", "cert-set", "manual TLS: cert store Set failed")
	c.warnManualOnce("api.example.com", "cert-set", "manual TLS: cert store Set failed")
	c.warnManualOnce("api.example.com", "cert-set", "manual TLS: cert store Set failed")

	assert.Len(t, c.manualLogged, 1, "same (host, errKind) repeated should occupy exactly one map entry")
}

// Different errKinds for the same host get separate entries: an
// operator who first hits "parse failed" and then fixes that into
// "missing keys" sees both transitions.
func TestWarnManualOnce_DifferentErrKindsAreSeparate(t *testing.T) {
	c := &Controller{
		cfg:          Config{Logger: log.NewLogger()},
		manualLogged: map[string]time.Time{},
	}
	c.warnManualOnce("api.example.com", "ref-parse", "first")
	c.warnManualOnce("api.example.com", "missing-keys", "second")
	c.warnManualOnce("api.example.com", "cert-set", "third")

	assert.Len(t, c.manualLogged, 3, "distinct errKinds occupy separate map entries")
}

// On a successful push, clearManualWarns drops every dedup entry for
// the host so the next failure (e.g. after a bad rotation) logs
// immediately rather than being suppressed by the prior window.
func TestClearManualWarns_DropsAllHostEntries(t *testing.T) {
	c := &Controller{
		cfg:          Config{Logger: log.NewLogger()},
		manualLogged: map[string]time.Time{},
	}
	c.warnManualOnce("api.example.com", "ref-parse", "x")
	c.warnManualOnce("api.example.com", "missing-keys", "x")
	c.warnManualOnce("other.example.com", "cert-set", "x")

	c.clearManualWarns("api.example.com")
	assert.Len(t, c.manualLogged, 1, "only the unrelated other.example.com entry should survive")
	_, has := c.manualLogged["other.example.com::cert-set"]
	assert.True(t, has)
}
