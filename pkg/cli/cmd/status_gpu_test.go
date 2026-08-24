package cmd

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderCluster writes to a *os.File, so capture through a pipe. Read to
// EOF rather than once: renderCluster makes several Fprintf calls and a
// single Read would race them.
func captureCluster(t *testing.T, c *clusterReport) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	renderCluster(w, c)
	require.NoError(t, w.Close())
	out := <-done
	require.NoError(t, r.Close())
	return out
}

// The zero-change guarantee at the render layer: with no GPU signal
// gathered there is no GPU line, not a "none". A feature must not
// announce itself to someone who did not ask for it (RUNE-301 §12.4a).
func TestRenderCluster_NoGPULineWhenSignalAbsent(t *testing.T) {
	out := captureCluster(t, &clusterReport{
		ServerVersion: "v0.0.1-dev.150",
		Runners:       map[string]string{"docker": "ready"},
	})
	assert.NotContains(t, out, "GPU")
	assert.NotContains(t, strings.ToLower(out), "gpu")
	assert.Contains(t, out, "docker=ready")
}

func TestRenderCluster_GPULine(t *testing.T) {
	out := captureCluster(t, &clusterReport{
		ServerVersion: "v0.0.1-dev.150",
		GPUs:          map[string]string{"node-8f6a12cd": "2×NVIDIA L40S, probed 4m ago"},
	})
	assert.Contains(t, out, "GPUs:")
	assert.Contains(t, out, "2×NVIDIA L40S, probed 4m ago")
	assert.NotContains(t, out, "node-8f6a12cd", "one node needs no name prefix")
}

func TestRenderCluster_GPULineNamesEachNodeWhenSeveral(t *testing.T) {
	out := captureCluster(t, &clusterReport{
		GPUs: map[string]string{
			"gpu-1": "2×NVIDIA L40S, probed 4m ago",
			"gpu-2": "probe failed: nvidia-smi not found on PATH",
		},
	})
	assert.Contains(t, out, "gpu-1=2×NVIDIA L40S")
	assert.Contains(t, out, "gpu-2=probe failed")
}
