//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// TestExecProbe_PromotesToRunning is the dataplane regression for the redis
// "stuck Starting" bug. A service whose readiness probe is an exec command that
// PRINTS OUTPUT (redis-cli ping → "PONG") must still promote Starting → Running.
//
// The pre-fix failure: the Docker exec stream copies command output into an
// unbuffered io.Pipe that a health probe never reads, so stdcopy.StdCopy blocks
// forever on the write; ExecStream.Close() then deadlocks in wg.Wait() before it
// closes the pipe, so the probe's Execute never returns. Readiness never reports
// success (no promotion) and liveness never reports failure (no restart), so the
// instance strands in Starting with restarts=0 — exactly what prod showed.
//
// Mirrors infra/runeset/casts/redis.yaml's health block; the DO volume is
// dropped so the test runs on any Docker host (state is irrelevant here).
func TestExecProbe_PromotesToRunning(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	svcFile := filepath.Join(t.TempDir(), "redis.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: redis-exec
  image: "redis:7-alpine"
  imagePull: missing
  scale: 1
  args:
    - redis-server
    - --appendonly
    - "no"
  health:
    liveness:
      type: exec
      command: ["redis-cli", "ping"]
      initialDelaySeconds: 5
      intervalSeconds: 5
      timeoutSeconds: 3
    readiness:
      type: exec
      command: ["redis-cli", "ping"]
      initialDelaySeconds: 5
      intervalSeconds: 5
      timeoutSeconds: 3
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}

	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "redis-exec")

	inst := generated.NewInstanceServiceClient(ctx.Conn())
	// Image pull dominates first convergence on a cold runner; the exec probe
	// then has its 5s initial delay before it can promote.
	ctx.Eventually(3*time.Minute, "redis-exec to reach Running via its exec readiness probe", func() bool {
		return runningInstanceCount(t, ctx, inst, "redis-exec") == 1
	})

	// And it must STAY Running — a wedged exec probe that promotes late but then
	// hangs would not hold steady.
	stableUntil := time.Now().Add(8 * time.Second)
	for time.Now().Before(stableUntil) {
		if n := runningInstanceCount(t, ctx, inst, "redis-exec"); n != 1 {
			t.Fatalf("expected redis-exec to stay at 1 Running, got %d", n)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
