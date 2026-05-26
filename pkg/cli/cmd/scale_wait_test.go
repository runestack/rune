package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/stretchr/testify/assert"
)

func TestSplitRestartBudget_DefaultThirtySeventy(t *testing.T) {
	drain, start := splitRestartBudget(10*time.Minute, 0)
	// 30% of 10m = 3m, 70% = 7m
	assert.Equal(t, 3*time.Minute, drain)
	assert.Equal(t, 7*time.Minute, start)
}

func TestSplitRestartBudget_DrainOverride(t *testing.T) {
	drain, start := splitRestartBudget(5*time.Minute, 30*time.Second)
	assert.Equal(t, 30*time.Second, drain)
	assert.Equal(t, 5*time.Minute-30*time.Second, start)
}

func TestSplitRestartBudget_OverrideTooLarge(t *testing.T) {
	// drain-timeout >= timeout: clamp drain to (total - 10s) so start
	// still gets a chance to surface its own problems.
	drain, start := splitRestartBudget(60*time.Second, 90*time.Second)
	assert.Equal(t, 50*time.Second, drain)
	assert.Equal(t, 10*time.Second, start)
}

func TestSplitRestartBudget_TinyTimeoutPreservesStartFloor(t *testing.T) {
	// A 5s --timeout would otherwise leave start with ~3.5s. Start has
	// a 10s minimum.
	_, start := splitRestartBudget(5*time.Second, 0)
	assert.GreaterOrEqual(t, start, 10*time.Second)
}

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
