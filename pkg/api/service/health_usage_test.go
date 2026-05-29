package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func newHealthSvc() *HealthService {
	return &HealthService{logger: log.NewLogger()}
}

// readProcMeminfoUsed = MemTotal - MemAvailable (excludes reclaimable cache).
func TestReadProcMeminfoUsed(t *testing.T) {
	s := newHealthSvc()
	dir := t.TempDir()
	p := writeFile(t, dir, "meminfo", "MemTotal:       16384000 kB\nMemFree:         1000000 kB\nMemAvailable:    6384000 kB\nBuffers:          200000 kB\n")
	used, ok := s.readProcMeminfoUsed(p)
	require.True(t, ok)
	// (16384000 - 6384000) KiB * 1024
	assert.Equal(t, int64((16384000-6384000)*1024), used)
}

func TestReadProcMeminfoUsed_MissingFields(t *testing.T) {
	s := newHealthSvc()
	dir := t.TempDir()
	p := writeFile(t, dir, "meminfo", "MemTotal:       16384000 kB\n") // no MemAvailable
	_, ok := s.readProcMeminfoUsed(p)
	assert.False(t, ok)
}

// cgroup working set = memory.current - inactive_file.
func TestDetectMemoryUsedBytes_CgroupWorkingSet(t *testing.T) {
	s := newHealthSvc()
	dir := t.TempDir()
	cur := writeFile(t, dir, "memory.current", "1000000000\n")
	stat := writeFile(t, dir, "memory.stat", "anon 400000000\ninactive_file 300000000\nactive_file 50000000\n")

	gotCur, ok := s.readInt64File(cur)
	require.True(t, ok)
	assert.Equal(t, int64(1000000000), gotCur)
	assert.Equal(t, int64(300000000), s.readCgroupStatField(stat, "inactive_file"))
	assert.Equal(t, int64(0), s.readCgroupStatField(stat, "nonexistent"))
}

func TestReadCgroupCPUUsageUsec(t *testing.T) {
	s := newHealthSvc()
	dir := t.TempDir()
	p := writeFile(t, dir, "cpu.stat", "usage_usec 123456789\nuser_usec 100000000\nsystem_usec 23456789\n")
	v, ok := s.readCgroupCPUUsageUsec(p)
	require.True(t, ok)
	assert.Equal(t, int64(123456789), v)

	_, ok = s.readCgroupCPUUsageUsec(filepath.Join(dir, "missing"))
	assert.False(t, ok)
}

func TestClampPercent(t *testing.T) {
	assert.Equal(t, 0.0, clampPercent(-5))
	assert.Equal(t, 100.0, clampPercent(150))
	assert.Equal(t, 42.0, clampPercent(42))
}
