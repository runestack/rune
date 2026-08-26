package nodeinfo

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/hostcapacity"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSubsystem(t *testing.T, cfg Config) (*Subsystem, *repos.NodeRepo) {
	t.Helper()
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })

	repo := repos.NewNodeRepo(st)
	cfg.Repo = repo
	if cfg.NodeID == "" {
		cfg.NodeID = "node-8f6a12cd"
	}
	if cfg.Logger == nil {
		cfg.Logger = log.NewTestLogger()
	}
	sub, err := New(cfg)
	require.NoError(t, err)
	return sub, repo
}

func waitReady(t *testing.T, sub *Subsystem, within time.Duration) {
	t.Helper()
	select {
	case <-sub.Ready():
	case <-time.After(within):
		t.Fatalf("subsystem did not report ready within %s", within)
	}
}

// The GPU-less path: an empty device list, a stamped probe time, and no
// probe error. Empty inventory is the normal answer, not a failure.
func TestSubsystem_WritesEmptyInventory(t *testing.T) {
	sub, repo := newTestSubsystem(t, Config{Labels: map[string]string{"rune.io/role": "edge"}})
	ctx := context.Background()
	require.NoError(t, sub.Start(ctx))
	waitReady(t, sub, 5*time.Second)

	node, err := repo.Get(ctx, "node-8f6a12cd")
	require.NoError(t, err)
	assert.Empty(t, node.Devices)
	assert.Empty(t, node.DeviceProbeError)
	require.NotNil(t, node.DevicesProbedAt, "a probe that ran stamps DevicesProbedAt")
	assert.Equal(t, LocalNodeAddress, node.Address)
	assert.Equal(t, map[string]string{"rune.io/role": "edge"}, node.Labels)

	// Nothing refreshes Status or LastHeartbeat on a cadence, so the
	// record leaves them zero and the render omits them rather than
	// showing a blank status on a healthy box.
	assert.Empty(t, string(node.Status))
	assert.True(t, node.LastHeartbeat.IsZero())

	require.NoError(t, sub.Stop(ctx))
}

func TestSubsystem_WritesDevices(t *testing.T) {
	devices := []types.GPUDevice{{
		UUID: "GPU-8f6a", Index: 0, Vendor: "nvidia", Product: "NVIDIA L40S",
		VRAMBytes: 48 << 30, DriverVersion: "550.54.15", CUDAVersion: "12.4",
	}}
	sub, repo := newTestSubsystem(t, Config{Provider: StaticProvider("static", devices, nil)})
	ctx := context.Background()
	require.NoError(t, sub.Start(ctx))
	waitReady(t, sub, 5*time.Second)

	node, err := repo.Get(ctx, "node-8f6a12cd")
	require.NoError(t, err)
	require.Len(t, node.Devices, 1)
	assert.Equal(t, "GPU-8f6a", node.Devices[0].UUID)
	assert.Equal(t, int64(48<<30), node.Devices[0].VRAMBytes)
	assert.Empty(t, node.DeviceProbeError)
	require.NoError(t, sub.Stop(ctx))
}

// A probe error is recorded verbatim, because it is quoted back to the
// operator — without the field, six distinct causes collapse into "no
// devices".
func TestSubsystem_RecordsProbeError(t *testing.T) {
	sub, repo := newTestSubsystem(t, Config{
		Provider: StaticProvider("nvidia-smi", nil, errors.New("nvidia-smi not found on PATH")),
	})
	ctx := context.Background()
	require.NoError(t, sub.Start(ctx))
	waitReady(t, sub, 5*time.Second)

	node, err := repo.Get(ctx, "node-8f6a12cd")
	require.NoError(t, err)
	assert.Equal(t, "nvidia-smi not found on PATH", node.DeviceProbeError)
	assert.Empty(t, node.Devices)
	require.NotNil(t, node.DevicesProbedAt, "a probe that failed still ran")
	require.NoError(t, sub.Stop(ctx))
}

// A wedged driver must not hold the daemon down: Ready closes on the
// deadline, the record says so, and the stuck goroutine is abandoned
// rather than joined.
func TestSubsystem_ProbeTimeoutDoesNotBlockReady(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	sub, repo := newTestSubsystem(t, Config{
		Provider:      blockingProvider{release: block},
		ProbeDeadline: 50 * time.Millisecond,
	})
	ctx := context.Background()
	require.NoError(t, sub.Start(ctx))
	waitReady(t, sub, 5*time.Second)

	node, err := repo.Get(ctx, "node-8f6a12cd")
	require.NoError(t, err)
	assert.Equal(t, "probe timed out after 50ms", node.DeviceProbeError)
	require.NoError(t, sub.Stop(ctx))
}

// Start returns before the probe finishes. Agent.Start runs subsystem
// Starts serially and treats any error as fatal, so a slow probe here
// would keep every non-AI service on the box down.
func TestSubsystem_StartReturnsImmediately(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	sub, _ := newTestSubsystem(t, Config{
		Provider:      blockingProvider{release: block},
		ProbeDeadline: 5 * time.Second,
	})
	start := time.Now()
	require.NoError(t, sub.Start(context.Background()))
	assert.Less(t, time.Since(start), time.Second, "Start must not wait on the probe")
	require.NoError(t, sub.Stop(context.Background()))
}

// Nothing this package started outlives Stop. Asserted on the live stack
// dump rather than a goroutine count, so the claim is about THIS
// package's goroutines and not about whatever the store happens to be
// running.
//
// This is NOT the no-periodic-wakeup guard, and must not be mistaken for
// one: a ticker loop that exits on ctx cancel leaves nothing behind and
// would pass here. no_ticker_test.go is what forbids the ticker.
func TestSubsystem_NothingPeriodicOnAGPULessBox(t *testing.T) {
	sub, _ := newTestSubsystem(t, Config{})
	ctx := context.Background()
	require.NoError(t, sub.Start(ctx))
	waitReady(t, sub, 5*time.Second)
	require.NoError(t, sub.Stop(ctx))

	// Teardown is not instantaneous; poll until the package's goroutines
	// are gone, and fail only if they never go.
	deadline := time.Now().Add(5 * time.Second)
	for {
		remaining := goroutinesIn(t, "agent/nodeinfo")
		if remaining == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d nodeinfo goroutine(s) still running after Stop — a GPU-less "+
				"box must have nothing periodic left over:\n%s", remaining, stackDump(t))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// goroutinesIn counts live goroutines whose stack mentions pkg, minus the
// test goroutine that is calling this.
func goroutinesIn(t *testing.T, pkg string) int {
	t.Helper()
	n := 0
	for _, g := range strings.Split(stackDump(t), "\n\ngoroutine ") {
		if strings.Contains(g, pkg) && !strings.Contains(g, "_test.go") {
			n++
		}
	}
	return n
}

func stackDump(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

func TestNew_Validates(t *testing.T) {
	_, err := New(Config{NodeID: "n1"})
	assert.Error(t, err, "a nil Repo is rejected")

	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	_, err = New(Config{Repo: repos.NewNodeRepo(st)})
	assert.Error(t, err, "an empty NodeID is rejected")

	sub, err := New(Config{Repo: repos.NewNodeRepo(st), NodeID: "n1"})
	require.NoError(t, err)
	assert.Equal(t, "none", sub.cfg.Provider.Name(), "the default provider probes nothing")
	assert.Equal(t, DefaultProbeDeadline, sub.cfg.ProbeDeadline)
}

type blockingProvider struct{ release <-chan struct{} }

func (blockingProvider) Name() string { return "blocking" }

func (p blockingProvider) Probe(context.Context) ([]types.GPUDevice, error) {
	<-p.release
	return nil, nil
}

// A re-probe whose device set changed fires a Node event. The comparison
// is by device UUID, not by a Generation counter — types.Node has no
// such field, and the UUID set is the thing that actually changed.
func TestSubsystem_EmitsOnDeviceSetChange(t *testing.T) {
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	repo := repos.NewNodeRepo(st)

	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec, err := events.NewRecorder(db, log.NewTestLogger(), events.Options{})
	require.NoError(t, err)

	two := []types.GPUDevice{{UUID: "GPU-8f6a"}, {UUID: "GPU-2c11"}}
	run := func(devices []types.GPUDevice) {
		t.Helper()
		sub, err := New(Config{
			Repo: repo, NodeID: "node-1", Events: rec,
			Provider: StaticProvider("static", devices, nil), Logger: log.NewTestLogger(),
		})
		require.NoError(t, err)
		require.NoError(t, sub.Start(context.Background()))
		waitReady(t, sub, 5*time.Second)
		require.NoError(t, sub.Stop(context.Background()))
	}

	// First boot is not a change: there is nothing to have changed from.
	run(two)
	evs, err := rec.ListByResource(context.Background(), "", "Node", "node-1", 10)
	require.NoError(t, err)
	assert.Empty(t, evs, "first boot emits nothing")

	// Same set again: still nothing.
	run(two)
	evs, err = rec.ListByResource(context.Background(), "", "Node", "node-1", 10)
	require.NoError(t, err)
	assert.Empty(t, evs, "an unchanged device set emits nothing")

	// A card went away.
	run([]types.GPUDevice{{UUID: "GPU-8f6a"}})
	evs, err = rec.ListByResource(context.Background(), "", "Node", "node-1", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "GpuCapacityShrunk", evs[0].Reason)
	assert.Equal(t, types.EventLevelWarn, evs[0].Level)
	assert.Equal(t, "", evs[0].Namespace, "node events are cluster-scoped")
	assert.Contains(t, evs[0].Message, "GPU-2c11")
}

// Without an EventLog wired the subsystem is silent and does not crash.
func TestSubsystem_NilEventLogIsSafe(t *testing.T) {
	sub, repo := newTestSubsystem(t, Config{Provider: StaticProvider("s", []types.GPUDevice{{UUID: "GPU-1"}}, nil)})
	ctx := context.Background()
	require.NoError(t, sub.Start(ctx))
	waitReady(t, sub, 5*time.Second)
	_, err := repo.Get(ctx, "node-8f6a12cd")
	require.NoError(t, err)
	require.NoError(t, sub.Stop(ctx))
}

// The record carries the machine's size, from the same source the health
// service reports pressure against — two numbers that disagree would have
// a node reporting 80% of one and scheduling against the other.
func TestSubsystem_RecordsNodeCapacity(t *testing.T) {
	sub, repo := newTestSubsystem(t, Config{})
	ctx := context.Background()
	require.NoError(t, sub.Start(ctx))
	waitReady(t, sub, 5*time.Second)

	node, err := repo.Get(ctx, "node-8f6a12cd")
	require.NoError(t, err)
	assert.Greater(t, node.Resources.CPU, int64(0), "a machine running runed has CPU")
	assert.Greater(t, node.Resources.Memory, int64(0), "and memory")
	assert.Equal(t, hostcapacity.MillicoresFromCores(hostcapacity.CPUCores()), node.Resources.CPU,
		"the record and the health service must not disagree about the machine's size")

	// Allocatable is the number anything deciding placement wants, and it
	// must be strictly less than total — a node cannot offer its own
	// kernel's memory.
	assert.Greater(t, node.Resources.AllocatableMemory, int64(0))
	assert.Less(t, node.Resources.AllocatableMemory, node.Resources.Memory,
		"placing against total is how a request for the whole box gets placed on it")
	assert.Less(t, node.Resources.AllocatableCPU, node.Resources.CPU)

	require.NoError(t, sub.Stop(ctx))
}
