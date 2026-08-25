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
