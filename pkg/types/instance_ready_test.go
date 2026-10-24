package types

import (
	"testing"
	"time"
)

// `rune get instances -w` redraws on Equals == false; a readiness flip must
// count as a visible change.
func TestInstanceEquals_ReadinessFlipIsVisible(t *testing.T) {
	changed := time.Now()
	a := &Instance{ID: "i", Status: InstanceStatusRunning}
	b := &Instance{ID: "i", Status: InstanceStatusRunning, Ready: false, ReadyChangedAt: &changed}
	if a.Equals(b) {
		t.Fatal("ready → not ready must not compare equal")
	}
}
