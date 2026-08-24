package service

import (
	"context"
	"testing"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func putNode(t *testing.T, st store.Store, node *types.Node) {
	t.Helper()
	require.NoError(t, repos.NewNodeRepo(st).Upsert(context.Background(), node))
}

func sectionLines(r *generated.DescribeResult, title string) []string {
	for _, sec := range r.Sections {
		if sec.Title == title {
			return sec.Lines
		}
	}
	return nil
}

func TestDescribeNode_RendersDevices(t *testing.T) {
	svc, st := newDescribeTestService(t)
	probedAt := time.Now()
	putNode(t, st, &types.Node{
		ID: "node-8f6a12cd", Address: "127.0.0.1",
		Labels: map[string]string{"rune.io/role": "edge"},
		Devices: []types.GPUDevice{
			{UUID: "GPU-2c11", Index: 1, Vendor: "nvidia", Product: "NVIDIA L40S",
				VRAMBytes: 48 << 30, DriverVersion: "550.54.15", CUDAVersion: "12.4"},
			{UUID: "GPU-8f6a", Index: 0, Vendor: "nvidia", Product: "NVIDIA L40S",
				VRAMBytes: 48 << 30, DriverVersion: "550.54.15", CUDAVersion: "12.4"},
		},
		DevicesProbedAt: &probedAt,
	})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{Kind: "node", Name: "node-8f6a12cd"})
	require.NoError(t, err)
	r := resp.Result
	assert.Equal(t, "Node", r.Kind)
	assert.Equal(t, "node-8f6a12cd", r.Name)

	// Nothing refreshes Status or LastHeartbeat, so neither is rendered
	// — a blank status reads as a dead node on a healthy box.
	assert.Empty(t, r.Status)
	assert.Empty(t, r.Reason)

	gpus := sectionLines(r, "GPUs")
	require.Len(t, gpus, 2)
	assert.Contains(t, gpus[0], "GPU-8f6a", "devices render in index order")
	assert.Contains(t, gpus[0], "NVIDIA L40S")
	assert.Contains(t, gpus[0], "48Gi")
	assert.Contains(t, gpus[0], "driver 550.54.15")
	assert.Contains(t, gpus[0], "CUDA 12.4")
	assert.Contains(t, gpus[1], "GPU-2c11")

	assert.Equal(t, []string{"rune.io/role: edge"}, sectionLines(r, "Labels"))
}

// A GPU-less box gets NO GPU section at all — not a "none" line. A
// feature must not announce itself to someone who did not ask for it.
func TestDescribeNode_GPULessBoxRendersNoGPUSection(t *testing.T) {
	svc, st := newDescribeTestService(t)
	probedAt := time.Now()
	putNode(t, st, &types.Node{ID: "node-1", Address: "127.0.0.1", DevicesProbedAt: &probedAt})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{Kind: "node", Name: "node-1"})
	require.NoError(t, err)
	assert.Nil(t, sectionLines(resp.Result, "GPUs"), "no devices means no GPU block")
}

// The three states behind "no GPUs" are distinct, and two of them say so.
func TestDescribeNode_ProbeStates(t *testing.T) {
	probedAt := time.Now()
	tests := []struct {
		name string
		node types.Node
		want string
	}{
		{
			name: "never probed",
			node: types.Node{ID: "n", Address: "127.0.0.1"},
			want: "not probed yet",
		},
		{
			name: "probe failed",
			node: types.Node{ID: "n", Address: "127.0.0.1", DevicesProbedAt: &probedAt,
				DeviceProbeError: "nvidia-smi: permission denied (/dev/nvidiactl)"},
			want: "probe failed: nvidia-smi: permission denied (/dev/nvidiactl)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, st := newDescribeTestService(t)
			node := tt.node
			putNode(t, st, &node)
			resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{Kind: "node", Name: "n"})
			require.NoError(t, err)
			assert.Equal(t, []string{tt.want}, sectionLines(resp.Result, "GPUs"))
		})
	}
}

// Node events live under the empty-namespace key the record is stored
// under. Without attachEvents passing "", the trail is write-only.
func TestDescribeNode_FoldsClusterScopedEvents(t *testing.T) {
	st := store.NewTestStore()
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec, err := events.NewRecorder(db, log.GetDefaultLogger(), events.Options{})
	require.NoError(t, err)

	putNode(t, st, &types.Node{ID: "node-1", Address: "127.0.0.1"})
	require.NoError(t, rec.Emit(context.Background(), types.Event{
		Namespace: "", Kind: "Node", Name: "node-1", UID: "node-1",
		Level: types.EventLevelWarn, Reason: "GpuCapacityShrunk",
		Message: "device set changed on re-probe",
	}))

	svc := NewDescribeService(st, rec, log.GetDefaultLogger())
	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{Kind: "node", Name: "node-1"})
	require.NoError(t, err)
	require.Len(t, resp.Result.Events, 1)
	assert.Contains(t, resp.Result.Events[0].Message, "GpuCapacityShrunk")
}

// An operator on a pre-existing install has no way to discover the
// minted node-<hex> until `rune get nodes` lands with RUNE-304, so the
// old literal resolves to the single local node.
func TestDescribeNode_ResolvesLocalAlias(t *testing.T) {
	svc, st := newDescribeTestService(t)
	putNode(t, st, &types.Node{ID: "node-8f6a12cd", Address: "127.0.0.1"})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{Kind: "node", Name: "local"})
	require.NoError(t, err)
	assert.Equal(t, "node-8f6a12cd", resp.Result.Name)
}

// Describe ignores namespace for nodes: the record is cluster-scoped, so
// a namespace-pinned caller reads the same hardware inventory. There is
// nothing per-namespace on the record to redact — once reservations exist
// they will need one, and this test is what shows there is nothing to
// leak today.
func TestDescribeNode_IgnoresCallerNamespace(t *testing.T) {
	svc, st := newDescribeTestService(t)
	probedAt := time.Now()
	putNode(t, st, &types.Node{ID: "node-1", Address: "127.0.0.1", DevicesProbedAt: &probedAt,
		Devices: []types.GPUDevice{{UUID: "GPU-1", Product: "NVIDIA L40S", VRAMBytes: 48 << 30}}})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "node", Name: "node-1", Namespace: "prod",
	})
	require.NoError(t, err)
	assert.Equal(t, "node-1", resp.Result.Name)

	// Every rendered line is hardware. Nothing here names a namespace,
	// a service or a workload.
	for _, line := range sectionLines(resp.Result, "GPUs") {
		assert.NotContains(t, line, "prod")
	}
}

// `rune get events --for node/<name>` must find the node's events even
// when the caller has a default namespace configured. The record is
// stored under the empty-namespace key, so honouring the caller's
// namespace here made the node's event log write-only.
//
// The rescope is server-side deliberately: the namespace the caller sent
// still reaches RBAC, so a namespace-pinned grant keeps the access it
// has today. Doing it in the CLI instead would send "" to the
// authorizer, which denies any grant pinned to a namespace — turning a
// silent empty list into a permission error for exactly the operator the
// fix was for.
func TestListEvents_NodeIsClusterScoped(t *testing.T) {
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec, err := events.NewRecorder(db, log.GetDefaultLogger(), events.Options{})
	require.NoError(t, err)

	require.NoError(t, rec.Emit(context.Background(), types.Event{
		Namespace: "", Kind: "Node", Name: "node-1", UID: "node-1",
		Level: types.EventLevelWarn, Reason: "GpuCapacityShrunk", Message: "a card went away",
	}))
	require.NoError(t, rec.Emit(context.Background(), types.Event{
		Namespace: "prod", Kind: "Instance", Name: "api-0", UID: "i-1",
		Level: types.EventLevelInfo, Reason: "Started", Message: "started",
	}))

	svc := NewEventService(rec, log.GetDefaultLogger())

	// A caller with a default namespace still finds the node's events.
	resp, err := svc.ListEvents(context.Background(), &generated.ListEventsRequest{
		For: "node/node-1", Namespace: "prod",
	})
	require.NoError(t, err)
	require.Len(t, resp.Events, 1)
	assert.Equal(t, "GpuCapacityShrunk", resp.Events[0].Reason)

	// Other kinds are unchanged: still scoped to the caller's namespace.
	resp, err = svc.ListEvents(context.Background(), &generated.ListEventsRequest{
		For: "instance/api-0", Namespace: "staging",
	})
	require.NoError(t, err)
	assert.Empty(t, resp.Events, "a non-node kind still honours the caller's namespace")
}
