package nodeinfo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNVIDIACSV(t *testing.T) {
	// Real `nvidia-smi --query-gpu=uuid,index,name,memory.total,driver_version
	// --format=csv,noheader,nounits` output shape.
	const out = `GPU-8f6a1234-5678-90ab-cdef-1234567890ab, 0, NVIDIA L40S, 46068, 550.54.15
GPU-2c119876-5432-10fe-dcba-0987654321fe, 1, NVIDIA L40S, 46068, 550.54.15
`
	devices, err := parseNVIDIACSV(out)
	require.NoError(t, err)
	require.Len(t, devices, 2)

	assert.Equal(t, "GPU-8f6a1234-5678-90ab-cdef-1234567890ab", devices[0].UUID)
	assert.Equal(t, 0, devices[0].Index)
	assert.Equal(t, "nvidia", devices[0].Vendor)
	assert.Equal(t, "NVIDIA L40S", devices[0].Product)
	assert.Equal(t, int64(46068)*1024*1024, devices[0].VRAMBytes)
	assert.Equal(t, "550.54.15", devices[0].DriverVersion)
	assert.Equal(t, 1, devices[1].Index)
}

func TestParseNVIDIACSV_Empty(t *testing.T) {
	devices, err := parseNVIDIACSV("")
	require.NoError(t, err)
	assert.Empty(t, devices, "no rows is an empty inventory, not an error")
}

// CSV drift on a new driver version is one of the causes
// DeviceProbeError exists to name, so a malformed row is an error rather
// than a silent skip.
func TestParseNVIDIACSV_Rejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"too few fields", "GPU-1, 0, NVIDIA L40S\n", "nvidia-smi CSV"},
		{"non-numeric index", "GPU-1, zero, NVIDIA L40S, 46068, 550.54.15\n", "index"},
		{"non-numeric memory", "GPU-1, 0, NVIDIA L40S, lots, 550.54.15\n", "memory.total"},
		{"empty uuid", " , 0, NVIDIA L40S, 46068, 550.54.15\n", "empty uuid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseNVIDIACSV(tt.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// A device list past the cap is rejected, not truncated: a truncated
// inventory is a silently wrong answer (RUNE-301 §5.2).
func TestParseNVIDIACSV_RejectsRatherThanTruncates(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= maxProbeRows; i++ {
		b.WriteString("GPU-x, 0, NVIDIA L40S, 46068, 550.54.15\n")
	}
	_, err := parseNVIDIACSV(b.String())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than")
}

// nvidia-smi absent from PATH must read as itself, not as "no devices".
func TestNVIDIASMIProvider_MissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	devices, err := NVIDIASMIProvider().Probe(context.Background())
	require.Error(t, err)
	assert.Equal(t, "nvidia-smi not found on PATH", err.Error())
	assert.Nil(t, devices)
}

// A driver that refuses the query surfaces the driver's own words —
// that string is what `rune status` and the cast error quote verbatim.
func TestNVIDIASMIProvider_SurfacesStderr(t *testing.T) {
	dir := t.TempDir()
	writeFakeSMI(t, dir, "#!/bin/sh\necho 'Failed to initialize NVML: Driver/library version mismatch' >&2\nexit 1\n")
	t.Setenv("PATH", dir)

	_, err := NVIDIASMIProvider().Probe(context.Background())
	require.Error(t, err)
	assert.Equal(t, "nvidia-smi: Failed to initialize NVML: Driver/library version mismatch", err.Error())
}

func TestNVIDIASMIProvider_ParsesRealInvocation(t *testing.T) {
	dir := t.TempDir()
	writeFakeSMI(t, dir, "#!/bin/sh\necho 'GPU-8f6a, 0, NVIDIA L40S, 46068, 550.54.15'\n")
	t.Setenv("PATH", dir)

	devices, err := NVIDIASMIProvider().Probe(context.Background())
	require.NoError(t, err)
	require.Len(t, devices, 1)
	assert.Equal(t, "GPU-8f6a", devices[0].UUID)
}

func TestSelectProvider(t *testing.T) {
	t.Run("none is always the null provider", func(t *testing.T) {
		p, err := SelectProvider("none")
		require.NoError(t, err)
		assert.Equal(t, "none", p.Name())
	})

	t.Run("auto selects nothing when nvidia-smi is absent", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		p, err := SelectProvider("auto")
		require.NoError(t, err)
		assert.Equal(t, "none", p.Name(), "a machine with no driver tooling probes nothing at all")
	})

	t.Run("auto selects nvidia-smi when present", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSMI(t, dir, "#!/bin/sh\nexit 0\n")
		t.Setenv("PATH", dir)
		p, err := SelectProvider("")
		require.NoError(t, err)
		assert.Equal(t, "nvidia-smi", p.Name())
	})

	t.Run("unknown is an error", func(t *testing.T) {
		_, err := SelectProvider("nvml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown gpu provider")
	})
}

func writeFakeSMI(t *testing.T, dir, script string) {
	t.Helper()
	path := filepath.Join(dir, "nvidia-smi")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
}
