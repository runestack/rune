package hostcapacity

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// A cgroup quota must win over the host core count: inside a container
// the host's core count is capacity this process cannot use, and
// advertising it would have the node offer what the kernel will refuse.
func TestReadCgroupCPUMax(t *testing.T) {
	q, p, ok := readCgroupCPUMax(write(t, "cpu.max", "150000 100000\n"))
	require.True(t, ok)
	assert.EqualValues(t, 150000, q)
	assert.EqualValues(t, 100000, p)
	assert.InDelta(t, 1.5, float64(q)/float64(p), 0.001)
}

// "max" means no quota — fall through to the host, do not report zero.
func TestReadCgroupCPUMax_Unlimited(t *testing.T) {
	q, _, ok := readCgroupCPUMax(write(t, "cpu.max", "max 100000\n"))
	require.True(t, ok)
	assert.EqualValues(t, 0, q, "an unlimited quota must not be read as a zero-core limit")
}

func TestReadCgroupCPUMax_Missing(t *testing.T) {
	_, _, ok := readCgroupCPUMax(filepath.Join(t.TempDir(), "absent"))
	assert.False(t, ok)
}

func TestReadCgroupMemoryMax(t *testing.T) {
	v, ok := readCgroupMemoryMax(write(t, "memory.max", "2147483648\n"))
	require.True(t, ok)
	assert.EqualValues(t, 2<<30, v)

	_, ok = readCgroupMemoryMax(write(t, "memory.max", "max\n"))
	assert.False(t, ok, "unlimited must fall through rather than report a bound")
}

func TestReadProcMeminfoTotal(t *testing.T) {
	const meminfo = "MemTotal:       16384000 kB\nMemFree:         100 kB\n"
	v, ok := readProcMeminfoTotal(write(t, "meminfo", meminfo))
	require.True(t, ok)
	assert.EqualValues(t, int64(16384000)*1024, v, "meminfo is kB and the record is bytes")
}

func TestReadProcMeminfoTotal_Garbage(t *testing.T) {
	_, ok := readProcMeminfoTotal(write(t, "meminfo", "MemTotal: not-a-number\n"))
	assert.False(t, ok)
}

// Best-effort means a usable answer, never a zero that something else
// will treat as "this machine has no CPU".
func TestCPUCores_AlwaysPositive(t *testing.T) {
	assert.Greater(t, CPUCores(), 0.0)
}

func TestMemoryBytes_PositiveOnSupportedHosts(t *testing.T) {
	got := MemoryBytes()
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		assert.Greater(t, got, int64(0), "capacity detection must work on the platforms runed ships for")
	}
}

func TestMillicoresFromCores(t *testing.T) {
	assert.EqualValues(t, 1500, MillicoresFromCores(1.5))
	assert.EqualValues(t, 0, MillicoresFromCores(0))
	assert.EqualValues(t, 0, MillicoresFromCores(-1), "a negative core count is not a negative limit")
}

// The reserve is proportional above a floor: absolute machine overhead
// does not scale linearly, but it is never negligible.
func TestReservedMemory(t *testing.T) {
	tests := []struct {
		name  string
		total int64
		want  int64
	}{
		// 10% of 24GB is 2.4GB, above the floor.
		{"large node uses the fraction", 24_000_000_000, 2_400_000_000},
		// 10% of 8GB is 800MB, under the floor — but the floor is itself
		// capped at an eighth of the machine, so the cap is what binds.
		{"small node uses the capped floor", 8_000_000_000, 8_000_000_000 / 8},
		{"zero total reserves nothing", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ReservedMemory(tt.total))
		})
	}
}

// The number that matters: a node cannot offer everything it has, so a
// request for the machine's whole size must not fit it.
func TestAllocatableMemory_LeavesHeadroom(t *testing.T) {
	const total = 24_000_000_000 // a nominal 24GB node
	alloc := AllocatableMemory(total)

	// The case that motivated this: requesting the advertised size must
	// not fit, or the OOM killer arbitrates instead of the scheduler.
	assert.Less(t, alloc, int64(total), "a node cannot offer its own kernel's memory")
	assert.Equal(t, total-ReservedMemory(total), alloc)
}

// Never a negative budget, which arithmetic downstream would read as
// room. Zero only when there is genuinely nothing to report.
func TestAllocatable_NeverNegative(t *testing.T) {
	for _, total := range []int64{0, 1, 100, 1 << 20, 1 << 30, 1 << 59} {
		assert.GreaterOrEqual(t, AllocatableMemory(total), int64(0),
			"total=%d produced a negative budget", total)
		assert.LessOrEqual(t, AllocatableMemory(total), total,
			"total=%d offered more than it has", total)
	}
	assert.EqualValues(t, 0, AllocatableMemory(0))
	assert.EqualValues(t, 0, AllocatableMillicores(0))
}

// The CPU reserve is capped against the machine, exactly as the memory
// floor is. A flat 200m takes everything from a 200m cgroup quota — and
// types.NodeResources documents zero as "unknown, not none available", so
// the render would drop the CPU line and present a known "none" as an
// unknown.
func TestAllocatableMillicores_CappedOnSmallQuotas(t *testing.T) {
	for _, total := range []int64{100, 200, 500, 1000} {
		got := AllocatableMillicores(total)
		assert.Greater(t, got, int64(0),
			"a machine with %dm of CPU offers some of it; zero reads as 'unknown'", total)
		assert.Less(t, got, total, "and never all of it")
	}
	// Above the cap the flat reserve takes over unchanged.
	assert.EqualValues(t, 8000-ReservedMillicores, AllocatableMillicores(8000))
}

// CPU is compressible, so it is reserved thinly: an over-generous CPU
// reserve costs real capacity to prevent a harm that does not occur.
func TestAllocatableMillicores(t *testing.T) {
	assert.EqualValues(t, 8000-ReservedMillicores, AllocatableMillicores(8000))
	assert.Less(t, ReservedMillicores, int64(1000), "reserving a whole core would be too much")
}

// The scenario this whole distinction exists for: a pool of nominal 24GB
// and 8GB nodes, and a service asking for 24Gi.
//
// Against TOTAL, the 24GB node looks like it might take it. Against
// ALLOCATABLE it plainly cannot, and neither can anything else — which is
// the honest answer, delivered by a scheduler rather than by the OOM
// killer at 3am.
func TestAllocatable_TwentyFourGiOnATwentyFourGBPool(t *testing.T) {
	const (
		big   = 24_000_000_000 // s-8vcpu-24gb
		small = 8_000_000_000  // s-4vcpu-8gb
		want  = 24 << 30       // "24Gi" as ParseMemory reads it
	)

	// 24Gi is already larger than a nominal 24GB node's TOTAL — decimal
	// GB versus binary Gi, before any reserve enters into it.
	assert.Greater(t, int64(want), int64(big),
		"24Gi is 25.8GB: it does not fit a 24GB node even ignoring overhead")

	for _, total := range []int64{big, small} {
		assert.Less(t, AllocatableMemory(total), int64(want),
			"no node in this pool can offer 24Gi")
	}

	// What the big node CAN offer, so the error message has a real number
	// to name.
	assert.Greater(t, AllocatableMemory(big), int64(20_000_000_000),
		"a 24GB node should still offer roughly 21GB, not be written off")
}

// The floor is capped against the machine. Uncapped, a 1Gi box reserves
// everything and reports zero allocatable — which the contract reads as
// "unknown", so the surfaces go silent exactly where a request is least
// likely to fit. These are the boxes Rune is for.
func TestReservedMemory_FloorIsCappedOnSmallMachines(t *testing.T) {
	tests := []struct {
		name   string
		total  int64
		maxPct float64
	}{
		{"1GB droplet", 1_000_000_000, 13},
		{"2GB droplet", 2_000_000_000, 13},
		{"4GB droplet", 4_000_000_000, 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ReservedMemory(tt.total)
			pct := float64(r) / float64(tt.total) * 100
			assert.Less(t, pct, tt.maxPct, "reserving %.1f%% of a small box is not a reserve, it is a tax", pct)
			assert.Greater(t, AllocatableMemory(tt.total), int64(0),
				"a machine Rune targets must offer something, or the surfaces read as 'unknown'")
		})
	}
}

// Above the cap the proportional term takes over unchanged.
func TestReservedMemory_FractionDominatesOnLargeMachines(t *testing.T) {
	const total int64 = 24_000_000_000
	assert.Equal(t, total/100*ReservedMemoryFraction, ReservedMemory(total))
}

// MemoryBytes admits values up to 1<<60, and total*10 overflows int64
// long before that — which would collapse the reserve to the floor.
func TestReservedMemory_DoesNotOverflow(t *testing.T) {
	// The value matters: at 1<<59 the multiply still fits, so a test using
	// it passes against the buggy form. This is the top of the range
	// MemoryBytes admits.
	const huge = int64(1)<<60 - 1
	r := ReservedMemory(huge)
	assert.Greater(t, r, ReservedMemoryFloor,
		"an overflowed fraction goes negative and lets the floor win")
	assert.Less(t, r, huge)
	assert.InDelta(t, float64(huge)/10, float64(r), float64(huge)/100,
		"the fraction must still apply at the top of the admitted range")
}
