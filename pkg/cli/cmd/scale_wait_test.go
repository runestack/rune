package cmd

import (
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/stretchr/testify/assert"
)

// (The splitRestartBudget tests were removed with the two-phase restart flow:
// restart is now a single server-side template restamp — issue #140 — so
// there is no drain/start budget to split.)

func TestScalingDetachError_FormatsProblemSummary(t *testing.T) {
	err := scalingDetachError("stalled instance(s)", 3, &generated.ScalingStatusResponse{
		RunningInstances:     1,
		PendingInstances:     0,
		FailedInstances:      0,
		StalledInstances:     2,
		TerminatingInstances: 0,
		Problems: []*generated.InstanceProblem{
			{Name: "api-0", Status: "Stalled", Reason: "ImagePullFailed", Message: "manifest unknown"},
			{Name: "api-1", Status: "Stalled", Reason: "ImagePullFailed", RestartCount: 5},
		},
	})
	msg := err.Error()
	assert.Contains(t, msg, "stalled instance(s)")
	assert.Contains(t, msg, "running=1")
	assert.Contains(t, msg, "stalled=2")
	assert.Contains(t, msg, "api-0 [Stalled] ImagePullFailed")
	assert.Contains(t, msg, "manifest unknown")
	assert.Contains(t, msg, "restarts=5")
	assert.Contains(t, msg, "rune describe instance")
}

func TestScalingDetachError_NoProblemsStillReadable(t *testing.T) {
	err := scalingDetachError("timeout", 0, &generated.ScalingStatusResponse{
		TerminatingInstances: 1,
	})
	msg := err.Error()
	assert.True(t, strings.HasPrefix(msg, "timeout"))
	assert.NotContains(t, msg, "Problem instances")
}
