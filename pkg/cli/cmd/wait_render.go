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

	// TTY-only spinner state.
	stopCh    chan struct{}
	doneCh    chan struct{}
	spinFrame int

	mu       sync.Mutex
	running  int32
	pending  int32
	hasState bool

	// Non-TTY dedup state.
	lastKey string
}

// spinnerFrames is a 10-frame braille spinner. Same set commonly used by
// kubectl, helm, etc. Looks fine in light and dark terminals.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func newPhaseRenderer(label string, target int) *phaseRenderer {
	out := os.Stdout
	return &phaseRenderer{
		label:   label,
		target:  utils.ToInt32NonNegative(target),
		out:     out,
		isTTY:   term.IsTerminal(int(out.Fd())),
		started: time.Now(),
	}
}

// start begins the spinner ticker (TTY mode only). Safe to call once.
func (r *phaseRenderer) start() {
	if !r.isTTY {
		return
	}
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		defer close(r.doneCh)
		for {
			select {
			case <-r.stopCh:
				return
			case <-ticker.C:
				r.mu.Lock()
				r.spinFrame = (r.spinFrame + 1) % len(spinnerFrames)
				r.renderTTYLocked()
				r.mu.Unlock()
			}
		}
	}()
}

// update reports the latest server state. Triggers a re-render in TTY mode
// and a print in non-TTY mode (only if the state actually changed).
func (r *phaseRenderer) update(running, pending int32) {
	r.mu.Lock()
	r.running = running
	r.pending = pending
	r.hasState = true
	if r.isTTY {
		r.renderTTYLocked()
		r.mu.Unlock()
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
	if r.isTTY && r.stopCh != nil {
		close(r.stopCh)
		<-r.doneCh
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
		// Clear the spinner line, then print the permanent entry.
		fmt.Fprint(r.out, "\r\033[K")
		fmt.Fprintf(r.out, "  %s %s   %s\n", symbol, r.label, format.Dim("%s", suffix))
	} else {
		state := "complete"
		if !ok {
			state = "failed"
		}
		fmt.Fprintf(r.out, "%s %s: %s in %s\n", taskTag(), strings.ToLower(r.label), state, suffix)
	}
}

// renderTTYLocked redraws the spinner line. Caller must hold r.mu.
func (r *phaseRenderer) renderTTYLocked() {
	frame := spinnerFrames[r.spinFrame]
	elapsed := formatDuration(time.Since(r.started))
	var detail string
	if r.hasState {
		detail = fmt.Sprintf("%d/%d ready", r.running, r.target)
		if r.pending > 0 {
			detail += fmt.Sprintf(" · %d pending", r.pending)
		}
	} else {
		detail = "waiting…"
	}
	line := fmt.Sprintf("  %s %s   %s   %s",
		format.Highlight("%s", frame),
		r.label,
		detail,
		format.Dim("(%s)", elapsed))
	// \r returns to col 0; \033[K erases to end of line so we don't leave
	// trailing characters when the new line is shorter than the previous one.
	fmt.Fprintf(r.out, "\r\033[K%s", line)
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
