package controllers

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParallelScaleRestartSpec_Converges is the RFC #129 acceptance test:
// "a concurrency stress test (parallel scale + status + restart on one
// service) shows zero lost updates". It runs the REAL service controller,
// reconciler + workqueue, and scaling controller against a TestStore, with a
// fake instance controller that persists records and an async health-style
// flipper promoting Starting→Running — then hammers ONE service from three
// concurrent writer families:
//
//	(a) immediate scale ops cycling {0, 1, 3, 5}    (the `rune scale` shape)
//	(b) restart bounces: op→0 then op→previous       (the `rune restart --detach` shape)
//	(c) spec updates bumping Env + Generation        (the `rune cast` shape)
//
// After the hammer stops, one final scaling op sets a known target; the
// service must converge to exactly that state and STAY there. TestStore's
// watch is lossy (buffered drop), so assertions are level-triggered
// convergence — which is precisely the property the design guarantees.
func TestParallelScaleRestartSpec_Converges(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short mode")
	}

	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)

	// Real scaling controller against the same store.
	scaling := NewScalingController(testStore, log.NewTestLogger())

	const ns = "default"
	const name = "stress-service"

	// The fake persists created instances as Starting and promotes them to
	// Running shortly after, health-controller style. Deletion removes the
	// record so scale-downs actually converge.
	var flipWG sync.WaitGroup
	stressCtx, stressCancel := context.WithCancel(ctx)
	defer stressCancel()

	fakeInstanceController.CreateInstanceFunc = func(ctx context.Context, svc *types.Service, instanceName string, ordinal int) (*types.Instance, error) {
		inst := &types.Instance{
			ID: instanceName, Name: instanceName,
			Namespace: svc.Namespace, ServiceID: svc.ID, ServiceName: svc.Name,
			Status:    types.InstanceStatusStarting,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := testStore.Create(ctx, types.ResourceTypeInstance, svc.Namespace, inst.ID, inst); err != nil {
			return nil, err
		}
		// Async readiness promotion (10–50ms), the promoteToRunningOnReady shape.
		flipWG.Add(1)
		go func(id string) {
			defer flipWG.Done()
			select {
			case <-time.After(time.Duration(10+rand.Intn(40)) * time.Millisecond):
			case <-stressCtx.Done():
				return
			}
			var cur types.Instance
			_ = testStore.UpdateFunc(context.Background(), types.ResourceTypeInstance, ns, id, &cur, func() error {
				if cur.Status != types.InstanceStatusStarting {
					return store.ErrSkipUpdate
				}
				cur.Status = types.InstanceStatusRunning
				return nil
			}, store.WithHealthController())
		}(inst.ID)
		return inst, nil
	}
	fakeInstanceController.DeleteInstanceFunc = func(ctx context.Context, instance *types.Instance) error {
		return testStore.Delete(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID)
	}

	require.NoError(t, controller.Start(ctx))
	defer func() { _ = controller.Stop() }()
	require.NoError(t, scaling.Start(ctx))
	defer scaling.Stop()

	// Seed the service at scale 1.
	service := &types.Service{
		ID: name, Name: name, Namespace: ns,
		Image: "stress:latest", Runtime: "container", Scale: 1,
		Env:      map[string]string{"REV": "0"},
		Metadata: &types.ServiceMetadata{Generation: 1},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, ns, name, service))

	// currentService reads the live record.
	currentService := func() types.Service {
		var got types.Service
		require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, ns, name, &got))
		return got
	}

	// ---- The hammer: 3 writer families for ~5s on ONE service ----
	const hammerDuration = 5 * time.Second
	deadline := time.Now().Add(hammerDuration)
	var hammerWG sync.WaitGroup
	var lastTarget atomic.Int64
	lastTarget.Store(1)

	// (a) scale cycler
	hammerWG.Add(1)
	go func() {
		defer hammerWG.Done()
		targets := []int{0, 1, 3, 5}
		for i := 0; time.Now().Before(deadline); i++ {
			target := targets[i%len(targets)]
			svc := currentService()
			_ = scaling.CreateScalingOperation(ctx, &svc, types.ScalingOperationParams{
				CurrentScale: svc.Scale, TargetScale: target, IntervalSeconds: 1,
			})
			lastTarget.Store(int64(target))
			time.Sleep(time.Duration(20+rand.Intn(60)) * time.Millisecond)
		}
	}()

	// (b) restart bouncer: 0 then previous, back to back
	hammerWG.Add(1)
	go func() {
		defer hammerWG.Done()
		for time.Now().Before(deadline) {
			svc := currentService()
			prev := svc.Scale
			if prev == 0 {
				prev = 1
			}
			_ = scaling.CreateScalingOperation(ctx, &svc, types.ScalingOperationParams{
				CurrentScale: svc.Scale, TargetScale: 0, IntervalSeconds: 1,
			})
			_ = scaling.CreateScalingOperation(ctx, &svc, types.ScalingOperationParams{
				CurrentScale: 0, TargetScale: prev, IntervalSeconds: 1,
			})
			lastTarget.Store(int64(prev))
			time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)
		}
	}()

	// (c) spec updater (cast shape): bump an env var + Generation atomically
	hammerWG.Add(1)
	go func() {
		defer hammerWG.Done()
		for i := 0; time.Now().Before(deadline); i++ {
			var cur types.Service
			_ = testStore.UpdateFunc(ctx, types.ResourceTypeService, ns, name, &cur, func() error {
				if cur.Env == nil {
					cur.Env = map[string]string{}
				}
				cur.Env["REV"] = fmt.Sprintf("%d", i)
				if cur.Metadata == nil {
					cur.Metadata = &types.ServiceMetadata{}
				}
				cur.Metadata.Generation++
				return nil
			}, store.WithSource(store.EventSourceAPI))
			time.Sleep(time.Duration(30+rand.Intn(70)) * time.Millisecond)
		}
	}()

	hammerWG.Wait()

	// ---- Settle: one final authoritative scale op to a known target ----
	const finalTarget = 2
	svc := currentService()
	require.NoError(t, scaling.CreateScalingOperation(ctx, &svc, types.ScalingOperationParams{
		CurrentScale: svc.Scale, TargetScale: finalTarget, IntervalSeconds: 1,
	}))

	countInstances := func() (total, running int) {
		var instances []types.Instance
		if err := testStore.List(ctx, types.ResourceTypeInstance, ns, &instances); err != nil {
			return -1, -1
		}
		for i := range instances {
			if instances[i].ServiceName != name {
				continue
			}
			total++
			if instances[i].Status == types.InstanceStatusRunning {
				running++
			}
		}
		return total, running
	}

	converged := func() bool {
		got := currentService()
		total, running := countInstances()
		return got.Scale == finalTarget &&
			total == finalTarget && running == finalTarget &&
			got.Status == types.ServiceStatusRunning &&
			got.Metadata != nil &&
			got.Metadata.ObservedGeneration == got.Metadata.Generation
	}

	// Must converge well inside one resync interval — i.e. event-driven.
	require.Eventually(t, converged, 10*time.Second, 25*time.Millisecond,
		"after the hammer, the service must converge to the final target (scale=%d, all Running, ObservedGen==Gen)", finalTarget)

	// ...and STAY converged (late writes / echoes must not disturb it).
	stressCancel() // stop pending flippers from waiting
	flipWG.Wait()
	holdUntil := time.Now().Add(2 * time.Second)
	for time.Now().Before(holdUntil) {
		require.True(t, converged(), "converged state must hold (no late-write regression)")
		time.Sleep(50 * time.Millisecond)
	}

	// Zero lost updates: the final desired scale is exactly the last op's
	// target — no writer's change was silently reverted.
	got := currentService()
	assert.Equal(t, finalTarget, got.Scale, "final scale must equal the last-issued target (no lost update)")
	t.Logf("converged: scale=%d status=%s gen=%d observedGen=%d",
		got.Scale, got.Status, got.Metadata.Generation, got.Metadata.ObservedGeneration)
}
