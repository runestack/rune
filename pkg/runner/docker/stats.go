package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
)

// Compile-time check: the docker runner provides live instance stats.
var _ runner.StatsProvider = (*DockerRunner)(nil)

// statsResultTTL short-circuits rapid repeat reads (e.g. the dashboard's
// Services and Instances screens both listing within the same second) so a
// burst of RPCs costs one daemon round-trip per container.
const statsResultTTL = 2 * time.Second

// statSample is the cached previous CPU counter pair for one container,
// plus the last computed usage so TTL hits can return it unchanged.
type statSample struct {
	cpuTotal  uint64 // cumulative container cpu time (ns)
	sysTotal  uint64 // cumulative host cpu time (ns)
	taken     time.Time
	lastUsage *types.InstanceUsage
}

// statsCache lives on the Runner; one entry per container ID.
type statsCache struct {
	mu sync.Mutex
	m  map[string]statSample
}

func newStatsCache() *statsCache {
	return &statsCache{m: make(map[string]statSample)}
}

// InstanceStats reports a live resource-usage sample for the instance's
// container, implementing runner.StatsProvider.
//
// Sampling strategy (keeps list RPCs fast after the first call):
//   - First read for a container: a blocking stats read (stream=false) —
//     the daemon primes it with two internal samples (~1s) so PreCPUStats
//     is valid and the very first answer is a real instantaneous percent.
//   - Subsequent reads: a ContainerStatsOneShot (single fast sample) with
//     the CPU delta computed against our cached previous counters — i.e.
//     average CPU over the interval since the last read.
//   - Reads within statsResultTTL return the previous computed sample
//     without touching the daemon.
func (r *DockerRunner) InstanceStats(ctx context.Context, instance *types.Instance) (*types.InstanceUsage, error) {
	if instance == nil || instance.ContainerID == "" {
		return nil, fmt.Errorf("docker stats: instance has no container id")
	}
	if r.client == nil {
		return nil, fmt.Errorf("docker stats: client not initialized")
	}
	id := instance.ContainerID

	r.stats.mu.Lock()
	prev, havePrev := r.stats.m[id]
	r.stats.mu.Unlock()

	if havePrev && time.Since(prev.taken) < statsResultTTL && prev.lastUsage != nil {
		u := *prev.lastUsage
		return &u, nil
	}

	var resp container.StatsResponseReader
	var err error
	if havePrev {
		resp, err = r.client.ContainerStatsOneShot(ctx, id)
	} else {
		resp, err = r.client.ContainerStats(ctx, id, false)
	}
	if err != nil {
		return nil, fmt.Errorf("docker stats: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body; a close failure cannot affect the decoded stats

	var sr container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("docker stats: decode: %w", err)
	}

	usage := computeUsage(&sr, prevCounters(havePrev, prev))

	r.stats.mu.Lock()
	r.stats.m[id] = statSample{
		cpuTotal:  sr.CPUStats.CPUUsage.TotalUsage,
		sysTotal:  sr.CPUStats.SystemUsage,
		taken:     time.Now(),
		lastUsage: usage,
	}
	// Opportunistic GC: drop entries not refreshed in 10 minutes so the
	// cache doesn't grow with dead container IDs.
	for k, s := range r.stats.m {
		if time.Since(s.taken) > 10*time.Minute {
			delete(r.stats.m, k)
		}
	}
	r.stats.mu.Unlock()

	out := *usage
	return &out, nil
}

// prevCounters adapts a cached sample into the (prevCPU, prevSys, ok) triple
// computeUsage wants.
func prevCounters(have bool, s statSample) *[2]uint64 {
	if !have {
		return nil
	}
	return &[2]uint64{s.cpuTotal, s.sysTotal}
}

// computeUsage turns one docker stats frame (plus optional previous counters)
// into an InstanceUsage. Pure — unit-tested directly.
//
// CPU: delta(container)/delta(host) × 100 = share of the whole host, the
// same denominator as node-level CPU. When prev is nil, the frame's own
// PreCPUStats are used (valid on a blocking stream=false read; zero on a
// one-shot, which yields cpu = -1 "unknown").
//
// Memory: usage minus inactive file cache (cgroup v2 "inactive_file",
// cgroup v1 "total_inactive_file") — the same correction `docker stats`
// applies so page cache doesn't count as application memory.
func computeUsage(sr *container.StatsResponse, prev *[2]uint64) *types.InstanceUsage {
	var prevCPU, prevSys uint64
	if prev != nil {
		prevCPU, prevSys = prev[0], prev[1]
	} else {
		prevCPU = sr.PreCPUStats.CPUUsage.TotalUsage
		prevSys = sr.PreCPUStats.SystemUsage
	}

	cpu := -1.0 // unknown until we have a valid counter pair
	if prevSys > 0 && sr.CPUStats.SystemUsage > prevSys && sr.CPUStats.CPUUsage.TotalUsage >= prevCPU {
		cpuDelta := float64(sr.CPUStats.CPUUsage.TotalUsage - prevCPU)
		sysDelta := float64(sr.CPUStats.SystemUsage - prevSys)
		if sysDelta > 0 {
			cpu = cpuDelta / sysDelta * 100.0
			if cpu < 0 {
				cpu = 0
			}
			if cpu > 100 {
				cpu = 100
			}
		}
	}

	memUsed := sr.MemoryStats.Usage
	if v, ok := sr.MemoryStats.Stats["inactive_file"]; ok && v < memUsed {
		memUsed -= v // cgroup v2
	} else if v, ok := sr.MemoryStats.Stats["total_inactive_file"]; ok && v < memUsed {
		memUsed -= v // cgroup v1
	}

	return &types.InstanceUsage{
		CPUPercent:    cpu,
		MemUsedBytes:  memUsed,
		MemLimitBytes: sr.MemoryStats.Limit,
	}
}
