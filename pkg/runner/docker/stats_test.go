package docker

import (
	"testing"

	"github.com/docker/docker/api/types/container"
)

// statsFrame builds a minimal StatsResponse for computeUsage tests.
func statsFrame(cpuTotal, sysTotal, preCPU, preSys, memUsage, memLimit uint64, stats map[string]uint64) *container.StatsResponse {
	sr := &container.StatsResponse{}
	sr.CPUStats.CPUUsage.TotalUsage = cpuTotal
	sr.CPUStats.SystemUsage = sysTotal
	sr.PreCPUStats.CPUUsage.TotalUsage = preCPU
	sr.PreCPUStats.SystemUsage = preSys
	sr.MemoryStats.Usage = memUsage
	sr.MemoryStats.Limit = memLimit
	sr.MemoryStats.Stats = stats
	return sr
}

// TestComputeUsage_HostShareCPU verifies cpu = delta(container)/delta(host)
// × 100 using the frame's own precpu (the blocking first-read path).
func TestComputeUsage_HostShareCPU(t *testing.T) {
	// container consumed 200ms of the host's 1000ms window → 20%
	sr := statsFrame(1_200_000_000, 11_000_000_000, 1_000_000_000, 10_000_000_000, 0, 0, nil)
	u := computeUsage(sr, nil)
	if u.CPUPercent < 19.9 || u.CPUPercent > 20.1 {
		t.Fatalf("cpu = %v, want ≈20", u.CPUPercent)
	}
}

// TestComputeUsage_PrevCounters verifies the cached-counter path used after
// the first read (one-shot frames have zeroed precpu).
func TestComputeUsage_PrevCounters(t *testing.T) {
	sr := statsFrame(1_500_000_000, 12_000_000_000, 0, 0, 0, 0, nil)
	prev := &[2]uint64{1_000_000_000, 10_000_000_000} // +0.5s container over +2s host → 25%
	u := computeUsage(sr, prev)
	if u.CPUPercent < 24.9 || u.CPUPercent > 25.1 {
		t.Fatalf("cpu = %v, want ≈25", u.CPUPercent)
	}
}

// TestComputeUsage_UnknownCPUOnFirstOneShot: a one-shot frame with zero
// precpu and no cached counters must report cpu = -1 (unknown), never 0.
func TestComputeUsage_UnknownCPUOnFirstOneShot(t *testing.T) {
	sr := statsFrame(1_000_000_000, 10_000_000_000, 0, 0, 0, 0, nil)
	if u := computeUsage(sr, nil); u.CPUPercent != -1 {
		t.Fatalf("cpu = %v, want -1 (unknown)", u.CPUPercent)
	}
}

// TestComputeUsage_MemInactiveFile verifies the docker-stats memory
// correction: usage minus inactive_file (v2) / total_inactive_file (v1).
func TestComputeUsage_MemInactiveFile(t *testing.T) {
	v2 := statsFrame(0, 0, 0, 0, 600, 1000, map[string]uint64{"inactive_file": 100})
	if u := computeUsage(v2, nil); u.MemUsedBytes != 500 || u.MemLimitBytes != 1000 {
		t.Fatalf("v2: used=%d limit=%d, want 500/1000", u.MemUsedBytes, u.MemLimitBytes)
	}
	v1 := statsFrame(0, 0, 0, 0, 600, 1000, map[string]uint64{"total_inactive_file": 200})
	if u := computeUsage(v1, nil); u.MemUsedBytes != 400 {
		t.Fatalf("v1: used=%d, want 400", u.MemUsedBytes)
	}
	// Correction larger than usage must not underflow.
	odd := statsFrame(0, 0, 0, 0, 50, 1000, map[string]uint64{"inactive_file": 100})
	if u := computeUsage(odd, nil); u.MemUsedBytes != 50 {
		t.Fatalf("underflow guard: used=%d, want 50", u.MemUsedBytes)
	}
}

// TestComputeUsage_Clamp verifies the 0–100 clamp on pathological counters.
func TestComputeUsage_Clamp(t *testing.T) {
	sr := statsFrame(30_000_000_000, 11_000_000_000, 1_000_000_000, 10_000_000_000, 0, 0, nil)
	if u := computeUsage(sr, nil); u.CPUPercent != 100 {
		t.Fatalf("cpu = %v, want clamped 100", u.CPUPercent)
	}
}
