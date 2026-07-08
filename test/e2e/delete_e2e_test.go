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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// castCachedService casts an nginx service that starts from the locally cached
// image (imagePull: missing) so the test never depends on registry
// connectivity — instances reach Running purely from cache. Waits for the
// desired scale to land.
func castCachedService(t *testing.T, ctx *harness.Context, svc *generated.ServiceServiceClient, name string, scale int) {
	t.Helper()
	file := filepath.Join(t.TempDir(), name+".yaml")
	body := "service:\n  name: " + name + "\n  image: nginx:alpine\n  imagePull: missing\n  scale: " +
		itoa(scale) + "\n  ports:\n    - name: http\n      port: 80\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", file, "--detach", "--release", name)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestDelete_TearsDownRealInstancesNoOrphan exercises RFC #129 Phase 4
// foreground deletion end-to-end against a real runed with real containers:
// `rune delete` tombstones the service and the single-writer reconciler tears
// down every instance, then removes the record — with no periodic orphan sweep
// involved (those were retired in Phase 4b). The service record can only vanish
// AFTER its instances are gone (the finalizer gate), so "service NotFound" plus
// "zero live instances" together prove the cascade left no orphan.
func TestDelete_TearsDownRealInstancesNoOrphan(t *testing.T) {
	ctx := harness.New(t)
	svc := generated.NewServiceServiceClient(ctx.Conn())
	inst := generated.NewInstanceServiceClient(ctx.Conn())

	castCachedService(t, ctx, &svc, "doomed", 2)

	// Wait for real, running instances before deleting.
	ctx.Eventually(harness.DefaultConvergeTimeout, "two live instances before delete", func() bool {
		return len(liveInstanceIDs(ctx, inst, "doomed")) == 2
	})

	// Delete through the real CLI.
	ctx.CLI.MustRun(t, "delete", "service", "doomed", "--force")

	// The service record must disappear entirely (the terminal transition of
	// the tombstone cascade).
	ctx.Eventually(harness.DefaultConvergeTimeout, "service record removed", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		_, err := svc.GetService(c, &generated.GetServiceRequest{Name: "doomed", Namespace: "default"})
		return status.Code(err) == codes.NotFound
	})

	// And no instances may remain. Because the record can only be removed once
	// instances are gone, this holds the moment the service is NotFound — assert
	// it, then hold to catch any resurrection (there is no sweep to lean on).
	if got := liveInstanceIDs(ctx, inst, "doomed"); len(got) != 0 {
		t.Fatalf("expected no live instances after delete, got %d: %v", len(got), got)
	}
	stableUntil := time.Now().Add(2 * time.Second)
	for time.Now().Before(stableUntil) {
		if got := liveInstanceIDs(ctx, inst, "doomed"); len(got) != 0 {
			t.Fatalf("an instance reappeared after delete (%d live) — teardown must be terminal", len(got))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestDelete_Idempotent verifies a re-issued delete on an in-flight teardown is
// a clean success, not an error (the tombstone is the idempotency key).
func TestDelete_Idempotent(t *testing.T) {
	ctx := harness.New(t)
	svc := generated.NewServiceServiceClient(ctx.Conn())
	inst := generated.NewInstanceServiceClient(ctx.Conn())

	castCachedService(t, ctx, &svc, "twice", 1)
	ctx.Eventually(harness.DefaultConvergeTimeout, "one live instance before delete", func() bool {
		return len(liveInstanceIDs(ctx, inst, "twice")) == 1
	})

	// Two deletes back to back must both succeed.
	ctx.CLI.MustRun(t, "delete", "service", "twice", "--force", "--detach")
	ctx.CLI.MustRun(t, "delete", "service", "twice", "--force", "--detach")

	ctx.Eventually(harness.DefaultConvergeTimeout, "service record removed", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		_, err := svc.GetService(c, &generated.GetServiceRequest{Name: "twice", Namespace: "default"})
		return status.Code(err) == codes.NotFound
	})
}
