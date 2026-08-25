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
