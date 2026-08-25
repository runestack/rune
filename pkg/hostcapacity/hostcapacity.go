// Package hostcapacity reports how much CPU and memory this machine has
// to offer.
//
// It exists as its own package because two unrelated callers need the
// same answer and must not disagree about it: the health service, which
// reports live pressure as a fraction of capacity, and the agent, which
// records capacity on the node's inventory record. Two implementations
// would eventually drift, and the symptom would be a node reporting 80%
// of one number while something else scheduled against another.
//
// SCALARS ONLY. GPUs are not here and should not be: they are a list of
// identities rather than a quantity — each has a UUID that reservations
// are keyed on — and probing one can fail in ways worth reporting and
// hang in ways worth abandoning, none of which is true of reading a core
// count. That probe lives behind an injectable provider in
// internal/agent/nodeinfo. Both write to the same node record.
//
// Everything here is best-effort and returns a usable answer rather than
// an error: a container that cannot read its own cgroup should report the
// host's figures, not fail.
package hostcapacity

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// CPUCores is the number of cores available to this process.
//
// A cgroup CPU quota wins over the host core count, because inside a
// container the host's core count is a number this process cannot
// actually use — reporting it would have the node advertise capacity the
// kernel will not give it.
func CPUCores() float64 {
	if quota, period, ok := readCgroupCPUMax("/sys/fs/cgroup/cpu.max"); ok {
		if quota > 0 && period > 0 {
			if eff := float64(quota) / float64(period); eff > 0 {
				return eff
			}
		}
	}
	return float64(runtime.NumCPU())
}

// MemoryBytes is the memory available to this process, or 0 when it
// cannot be determined.
func MemoryBytes() int64 {
	if v, ok := readCgroupMemoryMax("/sys/fs/cgroup/memory.max"); ok && v > 0 && v < (1<<60) {
		return v
	}
	if v, ok := readProcMeminfoTotal("/proc/meminfo"); ok {
		return v
	}
	if v, ok := readSysctlMemsize(); ok {
		return v
	}
	return 0
}

// System reserve: what a node keeps for itself and never offers to
// workloads.
//
// This is the same argument RUNE-301 D21 makes for GPU memory, one level
// up. A machine's total memory is not schedulable memory: the kernel, the
// page cache, runed and the agent all live in it, and none of them
// appears in any request. Placing against the total is how a 24GB service
// lands on a 24GB box and the OOM killer picks the workload rather than
// the cause.
//
// Memory: a proportional reserve, with a floor that is itself capped
// against the machine. The absolute overhead of a host does not scale
// linearly, so small boxes need more than 10% — but an uncapped 1Gi floor
// takes 54% of a 2GB droplet and ALL of a 1GB one, and those are the
// boxes Rune is for. The cap keeps the floor from exceeding an eighth of
// the machine:
//
//	 1GB  → 125MB   (12.5%, the cap — was 100%, leaving nothing)
//	 2GB  → 250MB   (12.5%, the cap — was 54%)
//	 8GiB → 1Gi     (12.5%; cap and floor coincide exactly here)
//	24GB  → 2.4GB   (10%, the fraction takes over)
//
// CPU is deliberately reserved far more thinly. It is compressible —
// contention makes everything slower rather than killing anything — so an
// over-generous CPU reserve costs real capacity to prevent a harm that
// does not occur.
//
// THESE DEFAULTS ARE A STARTING POINT, not a settled decision. Whatever
// ends up scheduling across nodes owns the number, and should ratify or
// replace it against real workloads.
const (
	// ReservedMemoryFloor is the minimum held back on any node.
	ReservedMemoryFloor int64 = 1 << 30 // 1Gi

	// ReservedMemoryFraction is held back above the floor, as a
	// percentage of total.
	ReservedMemoryFraction = 10

	// ReservedMemoryFloorDivisor caps the floor at 1/N of the machine, so
	// a small box is not mostly reserve.
	ReservedMemoryFloorDivisor int64 = 8

	// ReservedMillicores is held back for the node's own processes.
	ReservedMillicores int64 = 200
)

// ReservedMemory is the memory withheld from workloads on a node of the
// given total size.
func ReservedMemory(total int64) int64 {
	if total <= 0 {
		return 0
	}
	// Divide before multiplying: MemoryBytes admits values up to 1<<60,
	// and total*10 overflows int64 well before that.
	byFraction := total / 100 * ReservedMemoryFraction

	// The floor, capped against the machine so it cannot swallow a small
	// one. Without the cap a 1GB box reserves everything and reports zero
	// allocatable — which the contract reads as "unknown", so the surfaces
	// go silent exactly where a request is least likely to fit.
	floor := ReservedMemoryFloor
	if maxFloor := total / ReservedMemoryFloorDivisor; floor > maxFloor {
		floor = maxFloor
	}
	if byFraction < floor {
		byFraction = floor
	}
	if byFraction > total {
		return total
	}
	return byFraction
}

// AllocatableMemory is what a node can actually offer to workloads.
// Never negative: a machine too small to cover its own reserve offers
// nothing rather than a negative budget that arithmetic downstream would
// read as room.
func AllocatableMemory(total int64) int64 {
	if v := total - ReservedMemory(total); v > 0 {
		return v
	}
	return 0
}

// AllocatableMillicores is the CPU a node can offer to workloads.
func AllocatableMillicores(totalMillicores int64) int64 {
	if totalMillicores <= 0 {
		return 0
	}
	// Capped against the machine, exactly as the memory floor is. A flat
	// 200m takes everything from a 200m cgroup quota, and zero is
	// documented as "unknown, not none available" — so the surfaces would
	// drop the CPU line entirely and present a known "none" as an unknown.
	reserved := ReservedMillicores
	if maxReserve := totalMillicores / ReservedMemoryFloorDivisor; reserved > maxReserve {
		reserved = maxReserve
	}
	if v := totalMillicores - reserved; v > 0 {
		return v
	}
	return 0
}

// MillicoresFromCores converts a core count to the millicore unit the
// rest of the tree uses for CPU (1000m = 1 core).
func MillicoresFromCores(cores float64) int64 {
	if cores <= 0 {
		return 0
	}
	return int64(cores * 1000)
}

func readCgroupCPUMax(path string) (quota int64, period int64, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	parts := strings.Fields(string(data))
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "max" {
		p, _ := strconv.ParseInt(parts[1], 10, 64)
		return 0, p, true
	}
	q, err1 := strconv.ParseInt(parts[0], 10, 64)
	p, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return q, p, true
}

func readCgroupMemoryMax(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(data))
	if v == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func readProcMeminfoTotal(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}

func readSysctlMemsize() (int64, bool) {
	if runtime.GOOS != "darwin" {
		return 0, false
	}
	out, err := exec.Command("sysctl", "-n", "hw.memsize").CombinedOutput()
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(out))
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
