package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
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

// stripANSI removes colour/erase escape sequences so assertions can look at
// the text the user actually sees.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !((s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= 'a' && s[j] <= 'z')) {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TestPhaseRendererTTYAnimates is the regression for the reported bug: on a
// TTY the progress line must animate in place. `rune restart` printed the
// non-TTY format to everyone because it never used this renderer at all, so
// users saw appended "[rune] restarting: 0/1" lines and no spinner.
func TestPhaseRendererTTYAnimates(t *testing.T) {
	buf := &lockedBuffer{}
	r := newPhaseRendererFor("Restarting", 1, buf, true)
	r.start()
	r.update(0, 1)
	// Let the ticker advance through several frames.
	time.Sleep(6 * spinnerInterval)
	r.update(1, 0)
	r.finish(true, "")

	out := buf.String()
	if !strings.Contains(out, "\r") {
		t.Error("TTY output must redraw in place (expected a carriage return)")
	}
	// More than one distinct frame means it actually animated rather than
	// painting a single static line.
	seen := map[string]bool{}
	for _, f := range spinnerFrames {
		if strings.Contains(out, f) {
			seen[f] = true
		}
	}
	if len(seen) < 2 {
		t.Errorf("expected the spinner to advance through multiple frames, saw %d: %q", len(seen), stripANSI(out))
	}
	// The animated line must not survive into the final output.
	plain := stripANSI(out)
	last := plain[strings.LastIndex(plain, "\r")+1:]
	if strings.ContainsAny(last, strings.Join(spinnerFrames, "")) {
		t.Errorf("spinner frame leaked into the final line: %q", last)
	}
	if !strings.Contains(plain, "Restarting") || !strings.Contains(last, "✓") {
		t.Errorf("expected a permanent ✓ Restarting line, got %q", last)
	}
}

// TestPhaseRendererNoteDedupes: callers poll, so a warning that stays true
// would otherwise reprint every second and bury the progress line.
func TestPhaseRendererNoteDedupes(t *testing.T) {
	buf := &lockedBuffer{}
	r := newPhaseRendererFor("Restarting", 1, buf, false)
	for i := 0; i < 5; i++ {
		r.note("replacement instance(s) unhealthy: web-abc (Failed)")
	}
	r.note("a different problem")

	got := strings.Count(buf.String(), "unhealthy")
	if got != 1 {
		t.Errorf("repeated note should print once, printed %d times:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "a different problem") {
		t.Error("a distinct note must still print")
	}
}

// A renderer built as a struct literal (as older tests do) has a nil notes
// map; note() must not panic on it.
func TestPhaseRendererNoteNilMap(t *testing.T) {
	buf := &bytes.Buffer{}
	r := &phaseRenderer{label: "Starting", out: buf, isTTY: false, started: time.Now()}
	r.note("careful")
	if !strings.Contains(buf.String(), "careful") {
		t.Errorf("expected the note to print, got %q", buf.String())
	}
}

// TestRunWithSpinnerNonTTYTagged: the one-shot spinner's non-TTY line must
// carry the same [rune] tag as every other non-TTY progress line, so CI logs
// are uniformly greppable.
func TestRunWithSpinnerNonTTYTagged(t *testing.T) {
	// os.Stdout is not a terminal under `go test`, so this exercises the
	// non-TTY branch without any plumbing.
	buf := captureStdout(t, func() {
		if err := runWithSpinner("Applying…", func() error { return nil }); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if !strings.Contains(stripANSI(buf), "[rune]") {
		t.Errorf("non-TTY spinner line should carry the [rune] tag, got %q", buf)
	}
	if strings.Contains(buf, "…") {
		t.Errorf("the ellipsis belongs to the animated form only, got %q", buf)
	}
}

// TestRunWithSpinnerPropagatesError: the spinner must be transparent to fn.
func TestRunWithSpinnerPropagatesError(t *testing.T) {
	want := errors.New("apply failed")
	captureStdout(t, func() {
		if got := runWithSpinner("Applying…", func() error { return want }); !errors.Is(got, want) {
			t.Errorf("error not propagated: got %v, want %v", got, want)
		}
	})
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = wr
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, rd)
		done <- b.String()
	}()
	fn()
	_ = wr.Close()
	os.Stdout = old
	return <-done
}

// lockedBuffer is a bytes.Buffer safe for the spinner goroutine to write to
// while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestPhaseRendererFinishWithoutStartClearsLine guards the exact asymmetry
// that the paint/clear split can reintroduce: update() paints regardless of
// whether the animation is running, and callers can finish() without ever
// calling start() (scale/stop do this when the watch fails to open). If stop
// only cleared while active, the painted line would survive and the permanent
// ✓/✗ entry would be appended to it on the same row.
func TestPhaseRendererFinishWithoutStartClearsLine(t *testing.T) {
	buf := &lockedBuffer{}
	r := newPhaseRendererFor("Scaling", 2, buf, true)
	r.update(1, 1) // paints, though start() was never called
	r.finish(false, "watch failed")

	plain := stripANSI(buf.String())
	last := plain[strings.LastIndex(plain, "\r")+1:]
	if strings.ContainsAny(last, strings.Join(spinnerFrames, "")) {
		t.Errorf("progress line leaked into the final entry: %q", last)
	}
	if !strings.Contains(last, "✗") {
		t.Errorf("expected the ✗ entry on its own line, got %q", last)
	}
}
