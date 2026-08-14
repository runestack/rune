package format

import (
	"strings"
	"testing"

	"github.com/pterm/pterm"
)

// TestPTermStatusLabelVolumePhases pins the colour mapping for Volume phases.
// Before this, only "pending"/"failed" were recognised, so a volume table
// rendered Available / Bound / Provisioning / Released / Stalled all in plain
// white — an operator got no visual signal that a volume was stalled.
func TestPTermStatusLabelVolumePhases(t *testing.T) {
	// pterm strips styling when it detects a non-TTY; force it on so the test
	// asserts the intended colour rather than the environment's.
	pterm.EnableColor()

	green := pterm.NewStyle(pterm.FgGreen, pterm.Bold)
	yellow := pterm.NewStyle(pterm.FgYellow, pterm.Bold)
	red := pterm.NewStyle(pterm.FgRed, pterm.Bold)

	cases := []struct {
		status string
		want   *pterm.Style
	}{
		// Volume phases (pkg/types.VolumeStatus*)
		{"Available", green},
		{"Bound", green},
		{"Provisioning", yellow},
		{"Pending", yellow},
		{"Released", yellow},
		{"Stalled", red},
		{"Failed", red},
		// Existing service/instance statuses must keep their colours.
		{"Running", green},
		{"Deploying", yellow},
		{"Error", red},
	}

	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			got := PTermStatusLabel(tc.status)
			want := tc.want.Sprint(tc.status)
			if got != want {
				t.Errorf("PTermStatusLabel(%q) = %q, want %q", tc.status, got, want)
			}
			// The label must always still contain the raw status text.
			if !strings.Contains(got, tc.status) {
				t.Errorf("PTermStatusLabel(%q) dropped the status text: %q", tc.status, got)
			}
		})
	}
}

// TestPTermStatusLabelCaseInsensitive: statuses arrive in mixed case from
// different resource kinds; colouring must not depend on it.
func TestPTermStatusLabelCaseInsensitive(t *testing.T) {
	pterm.EnableColor()
	green := pterm.NewStyle(pterm.FgGreen, pterm.Bold)
	for _, s := range []string{"available", "AVAILABLE", "Available"} {
		if got, want := PTermStatusLabel(s), green.Sprint(s); got != want {
			t.Errorf("PTermStatusLabel(%q) = %q, want %q", s, got, want)
		}
	}
}

// TestPTermStatusLabelUnknown: an unrecognised status still renders (plain),
// never empty — a new phase must not vanish from the table.
func TestPTermStatusLabelUnknown(t *testing.T) {
	pterm.EnableColor()
	got := PTermStatusLabel("SomeFuturePhase")
	if !strings.Contains(got, "SomeFuturePhase") {
		t.Errorf("unknown status dropped its text: %q", got)
	}
}
