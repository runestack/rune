// RUNE-121 S6 — printInitStepProgress dedupes by (instance,step,status).
package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// TestPrintInitStepProgress_TransitionsAndDedupe verifies that we emit
// exactly one line per (instance, step, status) tuple as the service's
// init step state advances tick-by-tick. Two ticks at the same status
// must produce the same output the second time around — i.e. nothing
// new — because the seen map blocks duplicates.
func TestPrintInitStepProgress_TransitionsAndDedupe(t *testing.T) {
	svc := &types.Service{
		Namespace: "ns",
		Name:      "tb",
		Instances: []types.Instance{{
			Name: "tb-0",
			InitStates: []types.InitStepState{{
				Name:   "format",
				Status: types.InitStepStatusRunning,
			}},
		}},
	}

	seen := map[string]types.InitStepStatus{}
	var buf bytes.Buffer
	printInitStepProgress(&buf, svc, seen)
	first := buf.String()
	if !strings.Contains(first, "▶ init step \"format\" on tb-0") {
		t.Fatalf("expected Running line, got %q", first)
	}

	// Second call with the same state must NOT re-emit.
	buf.Reset()
	printInitStepProgress(&buf, svc, seen)
	if buf.Len() != 0 {
		t.Fatalf("expected no output on dedupe tick, got %q", buf.String())
	}

	// Advance to Succeeded → one new line.
	svc.Instances[0].InitStates[0].Status = types.InitStepStatusSucceeded
	buf.Reset()
	printInitStepProgress(&buf, svc, seen)
	out := buf.String()
	if !strings.Contains(out, "✓ init step \"format\" on tb-0 succeeded") {
		t.Fatalf("expected Succeeded line, got %q", out)
	}
}

// TestPrintInitStepProgress_FailedRendersReasonAndMessage covers the
// failure rendering path: reason is required (defaults to "Failed"),
// message renders as an indented second line when present.
func TestPrintInitStepProgress_FailedRendersReasonAndMessage(t *testing.T) {
	svc := &types.Service{
		Instances: []types.Instance{{
			Name: "i0",
			InitStates: []types.InitStepState{{
				Name:    "fmt",
				Status:  types.InitStepStatusFailed,
				Reason:  "ExitNonZero",
				Message: "exit code 9 after 3 attempts",
			}},
		}},
	}
	var buf bytes.Buffer
	printInitStepProgress(&buf, svc, map[string]types.InitStepStatus{})
	out := buf.String()
	if !strings.Contains(out, "✗ init step \"fmt\" on i0 failed: ExitNonZero") {
		t.Fatalf("missing failure line in %q", out)
	}
	if !strings.Contains(out, "exit code 9 after 3 attempts") {
		t.Fatalf("missing failure message in %q", out)
	}
}

// TestPrintInitStepProgress_SkippedDefaultsMessage ensures Skipped
// entries with an empty Message still render usefully.
func TestPrintInitStepProgress_SkippedDefaultsMessage(t *testing.T) {
	svc := &types.Service{
		Instances: []types.Instance{{
			Name: "i0",
			InitStates: []types.InitStepState{{
				Name:   "fmt",
				Status: types.InitStepStatusSkipped,
			}},
		}},
	}
	var buf bytes.Buffer
	printInitStepProgress(&buf, svc, map[string]types.InitStepStatus{})
	if !strings.Contains(buf.String(), "↷ init step \"fmt\" on i0: skipped") {
		t.Fatalf("expected default skipped message, got %q", buf.String())
	}
}

// TestPrintInitStepProgress_NilServiceIsNoop guards against a nil
// service crashing the cast wait loop on a transient API blip.
func TestPrintInitStepProgress_NilServiceIsNoop(t *testing.T) {
	var buf bytes.Buffer
	printInitStepProgress(&buf, nil, map[string]types.InitStepStatus{})
	if buf.Len() != 0 {
		t.Fatalf("expected empty output for nil service, got %q", buf.String())
	}
}
