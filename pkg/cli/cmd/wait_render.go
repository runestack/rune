package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/utils"
	"golang.org/x/term"
)

// phaseRenderer renders the progress of a single wait-for-scale phase
// (e.g. "Draining" or "Starting") with two output modes:
//
//   - TTY: an in-place updating line with a spinner that ticks regardless of
//     server updates, so the user sees activity even while counts are static.
//     The line is promoted to a permanent ✓/✗ entry on Finish.
//   - Non-TTY: dedupes status updates by (running, pending) so we only print
//     when something actually changes. Final line on Finish.
//
// Use one phaseRenderer per phase. Call Update from the watch loop; call
// Finish when the phase ends (success or error).
type phaseRenderer struct {
	label  string // e.g. "Draining"
	target int32  // target instance count for the phase
	out    io.Writer
	isTTY  bool

	started time.Time

	// TTY-only animation. nil in non-TTY mode.
	spin *spinner

	mu       sync.Mutex
	running  int32
	pending  int32
	hasState bool

	// Non-TTY dedup state.
	lastKey string

	// Dedup for note(): the same warning re-emitted on every poll is noise.
	notes map[string]bool
}

// spinnerFrames is a 10-frame braille spinner. Same set commonly used by
// kubectl, helm, etc. Looks fine in light and dark terminals.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerInterval is how often the animation advances. It is deliberately
// decoupled from how often a command polls or receives server updates: the
// frame advances on its own so the line still looks alive while counts sit
// unchanged, and a command polling once a second does not animate at 1 fps.
const spinnerInterval = 100 * time.Millisecond

// clearLine returns to column 0 and erases to end of line, so a shorter
// redraw cannot leave tail characters from a longer previous one.
const clearLine = "\r\033[K"

// spinner is the shared animation primitive behind every progress display in
// the CLI (phaseRenderer for the watch-based waits, runWithSpinner for
// one-shot blocking calls). It owns exactly one goroutine that repaints a
// single line in place.
//
// Callers supply render, which returns the current line WITHOUT the frame;
// the spinner prefixes the frame. render is always invoked on the spinner's
// goroutine, or under the same mutex via repaint, so implementations only
// need to be consistent with themselves.
type spinner struct {
	out    io.Writer
	render func(frame string) string

	mu     sync.Mutex
	frame  int
	active bool
	// painted records that something was drawn on the current line, so stop
	// knows to erase it even if the animation was never started. update() can
	// paint before start(), and a caller may finish() without ever starting
	// (e.g. the watch fails to open) — without this, that line would survive
	// and the permanent ✓/✗ entry would be appended to it.
	painted bool

	stopCh chan struct{}
	doneCh chan struct{}
}

func newSpinner(out io.Writer, render func(frame string) string) *spinner {
	return &spinner{out: out, render: render}
}

// start begins animating. Safe to call once; a second call is a no-op.
func (s *spinner) start() {
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return
	}
	s.active = true
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		defer close(s.doneCh)
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.mu.Lock()
				s.frame = (s.frame + 1) % len(spinnerFrames)
				s.paintLocked()
				s.mu.Unlock()
			}
		}
	}()
}

// repaint redraws immediately at the current frame, for when state changed
// and the caller does not want to wait up to spinnerInterval to show it.
func (s *spinner) repaint() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paintLocked()
}

func (s *spinner) paintLocked() {
	fmt.Fprintf(s.out, "%s%s", clearLine, s.render(spinnerFrames[s.frame]))
	s.painted = true
}

// clearLocked erases the animated line if one is on screen.
func (s *spinner) clearLocked() {
	if !s.painted {
		return
	}
	fmt.Fprint(s.out, clearLine)
	s.painted = false
}

// stop halts the animation and clears the line, leaving the cursor at column
// 0 so the caller can print a permanent line in its place. Safe to call
// without start, and safe to call twice.
func (s *spinner) stop() {
	s.mu.Lock()
	if !s.active {
		// Never started, or already stopped — still erase anything painted.
		s.clearLocked()
		s.mu.Unlock()
		return
	}
	s.active = false
	stopCh, doneCh := s.stopCh, s.doneCh
	s.mu.Unlock()

	close(stopCh)
	<-doneCh

	s.mu.Lock()
	s.clearLocked()
	s.mu.Unlock()
}

// interrupt clears the animated line, runs print (which may emit permanent
// output), then repaints. Without this, anything written mid-flight lands on
// top of the spinner line and garbles both.
func (s *spinner) interrupt(print func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLocked()
	print()
	// Only resume the animated line if one was there to begin with; otherwise
	// a note before start() would leave a stray frame behind.
	if s.active {
		s.paintLocked()
	}
}

func newPhaseRenderer(label string, target int) *phaseRenderer {
	out := os.Stdout
	return newPhaseRendererFor(label, target, out, term.IsTerminal(int(out.Fd())))
}

// newPhaseRendererFor builds a renderer against an explicit writer and mode,
// so both paths (including the animated one) are exercisable in tests.
func newPhaseRendererFor(label string, target int, out io.Writer, isTTY bool) *phaseRenderer {
	r := &phaseRenderer{
		label:   label,
		target:  utils.ToInt32NonNegative(target),
		out:     out,
		isTTY:   isTTY,
		started: time.Now(),
		notes:   map[string]bool{},
	}
	if isTTY {
		r.spin = newSpinner(out, r.line)
	}
	return r
}

// start begins the spinner ticker (TTY mode only). Safe to call once.
func (r *phaseRenderer) start() {
	if r.spin != nil {
		r.spin.start()
	}
}

// update reports the latest server state. Triggers a re-render in TTY mode
// and a print in non-TTY mode (only if the state actually changed).
func (r *phaseRenderer) update(running, pending int32) {
	r.mu.Lock()
	r.running = running
	r.pending = pending
	r.hasState = true
	if r.isTTY {
		r.mu.Unlock()
		// Repaint outside r.mu: the spinner takes its own lock and calls
		// back into r.line, which needs r.mu. Holding both here in the
		// opposite order would deadlock against the ticker goroutine.
		r.spin.repaint()
		return
	}
	// Non-TTY: dedupe.
	key := fmt.Sprintf("%d/%d/%d", running, pending, r.target)
	if key == r.lastKey {
		r.mu.Unlock()
		return
	}
	r.lastKey = key
	r.mu.Unlock()
	r.printNonTTYTransition(running, pending)
}

// finish stops the spinner (TTY) and prints a final ✓/✗ entry. Safe to call
// without a preceding start().
func (r *phaseRenderer) finish(ok bool, note string) {
	if r.spin != nil {
		r.spin.stop()
	}
	dur := time.Since(r.started)
	symbol := format.Success("✓")
	if !ok {
		symbol = format.Error("✗")
	}
	suffix := formatDuration(dur)
	if note != "" {
		suffix = note + " · " + suffix
	}
	if r.isTTY {
		// stop() already cleared the line; print the permanent entry in place.
		fmt.Fprintf(r.out, "  %s %s   %s\n", symbol, r.label, format.Dim("%s", suffix))
	} else {
		state := "complete"
		if !ok {
			state = "failed"
		}
		fmt.Fprintf(r.out, "%s %s: %s in %s\n", taskTag(), strings.ToLower(r.label), state, suffix)
	}
}

// line renders the progress line for the given spinner frame. Passed to the
// spinner as its render func, so it must take r.mu itself — the spinner
// already holds its own lock when it calls in.
func (r *phaseRenderer) line(frame string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	elapsed := formatDuration(time.Since(r.started))
	detail := "waiting…"
	if r.hasState {
		detail = fmt.Sprintf("%d/%d ready", r.running, r.target)
		if r.pending > 0 {
			detail += fmt.Sprintf(" · %d pending", r.pending)
		}
	}
	return fmt.Sprintf("  %s %s   %s   %s",
		format.Highlight("%s", frame),
		r.label,
		detail,
		format.Dim("(%s)", elapsed))
}

// note emits a permanent line alongside the progress display — a warning the
// operator should keep seeing after the phase moves on. It is deduped by
// text, because callers poll: without that, a warning that stays true prints
// once per poll and buries the progress line.
//
// In TTY mode the animated line is cleared first and repainted after, so the
// note lands cleanly instead of on top of the spinner.
func (r *phaseRenderer) note(text string) {
	r.mu.Lock()
	if r.notes == nil {
		// A renderer built as a struct literal has no map; don't panic.
		r.notes = map[string]bool{}
	}
	if r.notes[text] {
		r.mu.Unlock()
		return
	}
	r.notes[text] = true
	r.mu.Unlock()

	emit := func() { fmt.Fprintf(r.out, "  %s %s\n", format.Dim("⚠"), text) }
	if r.spin != nil {
		r.spin.interrupt(emit)
		return
	}
	emit()
}

// printNonTTYTransition prints one line per state change in non-TTY mode.
func (r *phaseRenderer) printNonTTYTransition(running, pending int32) {
	parts := []string{fmt.Sprintf("%d/%d ready", running, r.target)}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pending))
	}
	fmt.Fprintf(r.out, "%s %s: %s\n",
		taskTag(),
		strings.ToLower(r.label),
		strings.Join(parts, ", "))
}

// runWithSpinner runs fn while animating a single in-place line, for one-shot
// blocking calls that report no incremental progress (e.g. cast's apply).
// Where phaseRenderer shows counts converging, this only shows liveness.
//
// On a non-TTY it prints the label once, tagged like every other non-TTY
// progress line, and runs fn. The line is cleared before fn's result is
// rendered either way; fn's error is returned unchanged.
func runWithSpinner(label string, fn func() error) error {
	out := os.Stdout
	if !term.IsTerminal(int(out.Fd())) {
		fmt.Fprintf(out, "%s %s\n", taskTag(), strings.TrimSuffix(label, "…"))
		return fn()
	}
	started := time.Now()
	s := newSpinner(out, func(frame string) string {
		return fmt.Sprintf("  %s %s   %s",
			format.Highlight("%s", frame),
			label,
			format.Dim("(%s)", formatDuration(time.Since(started))))
	})
	s.start()
	err := fn()
	s.stop()
	return err
}

// taskTag is the bracketed prefix used in non-TTY output, e.g. "[rune]".
// Kept short and stable so it's easy to grep in CI logs.
func taskTag() string {
	return format.Dim("[rune]")
}

// formatDuration renders a duration as a compact "Xs" or "Xm Ys" string.
// We always show one decimal for sub-minute durations so users can see
// improvements between runs.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm %ds", mins, secs)
}
