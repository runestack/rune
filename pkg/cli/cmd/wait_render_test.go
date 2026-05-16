package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestPhaseRendererNonTTY verifies the non-TTY mode: each call to update only
// produces output when (running, pending) differ from the previous call. The
// final Finish line is always printed.
func TestPhaseRendererNonTTY(t *testing.T) {
	buf := &bytes.Buffer{}
	r := &phaseRenderer{
		label:   "Draining",
		target:  0,
		out:     buf,
		isTTY:   false, // force non-TTY for deterministic output
		started: time.Now(),
	}

	r.update(1, 0)
	r.update(1, 0) // duplicate — should not print
	r.update(1, 0) // duplicate — should not print
	r.update(0, 1)
	r.update(0, 1) // duplicate
	r.update(0, 0)
	r.finish(true, "")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")

	// Expected: 3 transition lines + 1 finish line = 4 lines.
	if got, want := len(lines), 4; got != want {
		t.Fatalf("got %d lines, want %d:\n%s", got, want, buf.String())
	}

	// Transitions should be distinct.
	for i := 0; i < 3; i++ {
		if !strings.Contains(lines[i], "draining:") {
			t.Errorf("line %d missing label: %q", i, lines[i])
		}
	}
	if lines[0] == lines[1] || lines[1] == lines[2] {
		t.Errorf("adjacent transition lines should differ, got:\n%s", buf.String())
	}

	// Finish line should report completion + a duration.
	last := lines[len(lines)-1]
	if !strings.Contains(last, "complete in") {
		t.Errorf("finish line should say 'complete in ...', got %q", last)
	}
}

// TestPhaseRendererNonTTYError verifies the error finish path.
func TestPhaseRendererNonTTYError(t *testing.T) {
	buf := &bytes.Buffer{}
	r := &phaseRenderer{
		label:   "Starting",
		target:  3,
		out:     buf,
		isTTY:   false,
		started: time.Now(),
	}
	r.update(1, 2)
	r.finish(false, "image pull failed")

	out := buf.String()
	if !strings.Contains(out, "failed in") {
		t.Errorf("expected 'failed in ...' in output, got:\n%s", out)
	}
}

// TestFormatDuration spot-checks the duration formatting used in both modes.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "0.5s"},
		{1500 * time.Millisecond, "1.5s"},
		{45 * time.Second, "45.0s"},
		{61 * time.Second, "1m 1s"},
		{3*time.Minute + 7*time.Second, "3m 7s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v): got %q, want %q", c.in, got, c.want)
		}
	}
}
