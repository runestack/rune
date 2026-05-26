package service

import (
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testService() *ServiceService {
	return &ServiceService{logger: log.NewTestLogger()}
}

func inst(name string, st types.InstanceStatus, opts ...func(*types.Instance)) *types.Instance {
	i := &types.Instance{Name: name, Status: st}
	for _, o := range opts {
		o(i)
	}
	return i
}

func withReason(failure, status string) func(*types.Instance) {
	return func(i *types.Instance) {
		i.FailureReason = failure
		i.StatusReason = status
	}
}

func withRestarts(n int) func(*types.Instance) {
	return func(i *types.Instance) {
		if i.Metadata == nil {
			i.Metadata = &types.InstanceMetadata{}
		}
		i.Metadata.RestartCount = n
	}
}

func TestSummarizeInstances_Counts(t *testing.T) {
	got := summarizeInstances([]*types.Instance{
		inst("a", types.InstanceStatusRunning),
		inst("b", types.InstanceStatusRunning),
		inst("c", types.InstanceStatusStarting),
		inst("d", types.InstanceStatusPending),
		inst("e", types.InstanceStatusFailed),
		inst("f", types.InstanceStatusStalled),
		inst("g", types.InstanceStatusTerminating),
		inst("h", types.InstanceStatusStopped),
	})
	assert.Equal(t, 2, got.running)
	assert.Equal(t, 2, got.pending)
	assert.Equal(t, 1, got.failed)
	assert.Equal(t, 1, got.stalled)
	assert.Equal(t, 1, got.terminating)
	// live = running + pending + terminating. Stopped/Failed/Stalled
	// are NOT live (Stopped is terminal). Terminating IS live so drain
	// completion waits for graceful shutdown to finish.
	assert.Equal(t, 5, got.live, "live must include Running+Pending+Terminating, exclude terminal/stopped")
}

func TestSummarizeInstances_ProblemPrioritization(t *testing.T) {
	got := summarizeInstances([]*types.Instance{
		// Mix in a healthy one so it doesn't get into problems.
		inst("ok-1", types.InstanceStatusRunning),
		// A Starting-with-restarts should rank below Failed/Stalled.
		inst("flaky", types.InstanceStatusStarting, withRestarts(3)),
		// Failed before Stalled in input order; severity pass must reorder.
		inst("crashed", types.InstanceStatusFailed, withReason("OOMKilled", "")),
		inst("wedged", types.InstanceStatusStalled, withReason("ImagePullFailed", "")),
	})
	require.Len(t, got.problems, 3)
	assert.Equal(t, "wedged", got.problems[0].Name, "Stalled should come first")
	assert.Equal(t, "ImagePullFailed", got.problems[0].Reason)
	assert.Equal(t, "crashed", got.problems[1].Name, "Failed second")
	assert.Equal(t, "flaky", got.problems[2].Name, "Starting+restarts third")
	assert.Equal(t, int32(3), got.problems[2].RestartCount)
}

func TestSummarizeInstances_BoundedToThree(t *testing.T) {
	var in []*types.Instance
	for i := 0; i < 10; i++ {
		in = append(in, inst("f"+string(rune('0'+i)), types.InstanceStatusFailed))
	}
	got := summarizeInstances(in)
	assert.Len(t, got.problems, maxProblemsInStream)
	assert.Equal(t, 10, got.failed, "count is unbounded; problems list is bounded")
}

func TestSummarizeInstances_StatusReasonFallback(t *testing.T) {
	// FailureReason empty → StatusReason wins (matches the live-instance
	// case where StatusReason is set on transitions but FailureReason is
	// only set at the moment of Failed).
	got := summarizeInstances([]*types.Instance{
		inst("pending", types.InstanceStatusStalled, withReason("", "VolumeNotReady")),
	})
	require.Len(t, got.problems, 1)
	assert.Equal(t, "VolumeNotReady", got.problems[0].Reason)
}

func TestSummarizeInstances_HealthyStartingNotAProblem(t *testing.T) {
	// Starting with 0 restarts is normal startup, not a problem.
	got := summarizeInstances([]*types.Instance{
		inst("booting", types.InstanceStatusStarting),
	})
	assert.Empty(t, got.problems)
	assert.Equal(t, 1, got.pending)
}

func TestIsScalingComplete_DrainCompleteWhenNoLive(t *testing.T) {
	// Stopped+Failed records linger but live==0 — drain is done.
	// Pre-fix this returned false because len(instances) > 0.
	s := testService()
	instances := []*types.Instance{
		inst("a", types.InstanceStatusStopped),
		inst("b", types.InstanceStatusFailed),
	}
	complete, target := s.isScalingComplete(
		&types.Service{Scale: 0},
		instances,
		summarizeInstances(instances),
		0,
	)
	assert.True(t, complete, "drain should complete when only Stopped/Failed remain")
	assert.Equal(t, int32(0), target)
}

func TestIsScalingComplete_DrainIncompleteWhileTerminating(t *testing.T) {
	// Terminating instances are still consuming resources; drain
	// must wait until they reach a terminal state.
	s := testService()
	instances := []*types.Instance{
		inst("a", types.InstanceStatusTerminating),
	}
	complete, _ := s.isScalingComplete(
		&types.Service{Scale: 0},
		instances,
		summarizeInstances(instances),
		0,
	)
	assert.False(t, complete, "drain must not complete while Terminating instances exist")
}

func TestIsScalingComplete_ScaleUpRequiresExactRunning(t *testing.T) {
	s := testService()
	instances := []*types.Instance{
		inst("a", types.InstanceStatusRunning),
		inst("b", types.InstanceStatusRunning),
		inst("c", types.InstanceStatusStarting),
	}
	complete, _ := s.isScalingComplete(
		&types.Service{Scale: 3},
		instances,
		summarizeInstances(instances),
		3,
	)
	assert.False(t, complete, "scale-up not done until all three are Running")
}

func TestIsScalingComplete_ScaleUpReady(t *testing.T) {
	s := testService()
	instances := []*types.Instance{
		inst("a", types.InstanceStatusRunning),
		inst("b", types.InstanceStatusRunning),
	}
	complete, _ := s.isScalingComplete(
		&types.Service{Scale: 2},
		instances,
		summarizeInstances(instances),
		2,
	)
	assert.True(t, complete)
}
