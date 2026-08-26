package service

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/hostcapacity"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gpuHealth(t *testing.T, nodes ...*types.Node) []*generated.ComponentHealth {
	t.Helper()
	st := store.NewTestStore()
	repo := repos.NewNodeRepo(st)
	for _, n := range nodes {
		require.NoError(t, repo.Upsert(context.Background(), n))
	}
	svc := NewHealthService(st, nil, log.GetDefaultLogger())
	resp, err := svc.GetHealth(context.Background(), &generated.GetHealthRequest{ComponentType: "gpu"})
	require.NoError(t, err)
	return resp.Components
}

// A machine with no GPUs and a clean probe produces NO component, so
// `rune status` emits no line — not "GPUs: none".
func TestGPUHealth_GPULessBoxReportsNothing(t *testing.T) {
	probedAt := time.Now()
	comps := gpuHealth(t, &types.Node{ID: "node-1", Address: "127.0.0.1", DevicesProbedAt: &probedAt})
	assert.Empty(t, comps)
}

// Neither does a node that has not been probed yet — "not known" is not
// a health signal to render.
func TestGPUHealth_UnprobedReportsNothing(t *testing.T) {
	comps := gpuHealth(t, &types.Node{ID: "node-1", Address: "127.0.0.1"})
	assert.Empty(t, comps)
}

func TestGPUHealth_ReportsDevices(t *testing.T) {
	probedAt := time.Now().Add(-4 * time.Minute)
	comps := gpuHealth(t, &types.Node{
		ID: "node-1", Address: "127.0.0.1", DevicesProbedAt: &probedAt,
		Devices: []types.GPUDevice{
			{UUID: "GPU-1", Product: "NVIDIA L40S"},
			{UUID: "GPU-2", Product: "NVIDIA L40S"},
		},
	})
	require.Len(t, comps, 1)
	assert.Equal(t, generated.HealthStatus_HEALTH_STATUS_HEALTHY, comps[0].Status)
	assert.Equal(t, "2×NVIDIA L40S, probed 4m ago", comps[0].Message)
}

// A driver that broke overnight was previously invisible until the next
// cast: runed runs at info and the only instrument was a debug line.
func TestGPUHealth_ReportsProbeFailure(t *testing.T) {
	probedAt := time.Now()
	comps := gpuHealth(t, &types.Node{
		ID: "node-1", Address: "127.0.0.1", DevicesProbedAt: &probedAt,
		DeviceProbeError: "nvidia-smi: permission denied (/dev/nvidiactl)",
	})
	require.Len(t, comps, 1)
	assert.Equal(t, generated.HealthStatus_HEALTH_STATUS_UNHEALTHY, comps[0].Status)
	assert.Equal(t, "probe failed: nvidia-smi: permission denied (/dev/nvidiactl)", comps[0].Message)
}

func TestGPUHealth_MixedProducts(t *testing.T) {
	probedAt := time.Now()
	comps := gpuHealth(t, &types.Node{
		ID: "node-1", Address: "127.0.0.1", DevicesProbedAt: &probedAt,
		Devices: []types.GPUDevice{
			{UUID: "GPU-1", Product: "NVIDIA L40S"},
			{UUID: "GPU-2", Product: "NVIDIA A100"},
			{UUID: "GPU-3", Product: "NVIDIA A100"},
		},
	})
	require.Len(t, comps, 1)
	assert.Contains(t, comps[0].Message, "2×NVIDIA A100")
	assert.Contains(t, comps[0].Message, "1×NVIDIA L40S")
}

// pkg/hostcapacity exists so this service and the agent's node record
// cannot disagree about how big the machine is — a node reporting eighty
// percent of one number while something schedules against another.
//
// That claim was unpinned: re-inlining detection into health.go, or
// changing it, left every package green. This crosses the boundary, so
// the two paths have to keep agreeing.
func TestHealthCapacityMatchesHostCapacity(t *testing.T) {
	svc := NewHealthService(store.NewTestStore(), nil, log.GetDefaultLogger())
	resp, err := svc.GetHealth(context.Background(), &generated.GetHealthRequest{ComponentType: "node"})
	require.NoError(t, err)
	require.Len(t, resp.Components, 1)

	got := resp.Components[0].GetResources()
	require.NotNil(t, got)

	assert.Equal(t, hostcapacity.CPUCores(), got.GetCpuCores(),
		"the health service and pkg/hostcapacity must report one CPU number")
	assert.Equal(t, hostcapacity.MemoryBytes(), got.GetMemTotalBytes(),
		"and one memory number")
}

// NOTE: there is deliberately no third test comparing the node record to
// the health service. One was written and deleted: it claimed to cross
// into internal/agent/nodeinfo and did not — it re-derived the value from
// hostcapacity inside the test, so mutating what the agent actually
// writes left it green.
//
// The property is genuinely pinned, transitively and by two tests that
// each cross a real boundary: the record equals hostcapacity
// (internal/agent/nodeinfo/subsystem_test.go, TestSubsystem_RecordsNodeCapacity)
// and the health service equals hostcapacity (above). If either drifts,
// one of them fails.
