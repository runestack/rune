package instance

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

// TestIsInstanceCompatible_ScaleBumpDoesNotRecreate is the regression test for
// issue #142: the scaling controller bumps Generation on every scale write
// (RFC #129 Phase 2), and the old compatibility rule compared the instance's
// recorded generation against Generation — so every scale op container-bounced
// its surviving instances. The check now compares TemplateGeneration, which
// scale never touches.
func TestIsInstanceCompatible_ScaleBumpDoesNotRecreate(t *testing.T) {
	ctx, _, testRunner, controller := setupTestController(t)

	// Service after several scale ops: Generation raced ahead, template untouched.
	service := &types.Service{
		ID: "svc-scaled", Name: "svc-scaled", Namespace: "default",
		Image: "app:v1", Scale: 3,
		Metadata: &types.ServiceMetadata{
			Generation:         7, // bumped by scale ops
			TemplateGeneration: 1, // template unchanged since creation
		},
	}
	// Survivor created at template generation 1, still running.
	instance := &types.Instance{
		ID: "svc-scaled-abc", Name: "svc-scaled-abc", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status:   types.InstanceStatusRunning,
		Metadata: &types.InstanceMetadata{ServiceGeneration: 1},
	}
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	compatible, reason := controller.IsInstanceCompatibleWithService(ctx, instance, service)
	assert.True(t, compatible,
		"a scale-only Generation bump must NOT recreate surviving instances (issue #142); got reason: %s", reason)
}

// TestIsInstanceCompatible_TemplateChangeRecreates: a cast that changes the
// template stamps TemplateGeneration, and instances recorded at an older
// template must be recreated.
func TestIsInstanceCompatible_TemplateChangeRecreates(t *testing.T) {
	ctx, _, testRunner, controller := setupTestController(t)

	service := &types.Service{
		ID: "svc-recast", Name: "svc-recast", Namespace: "default",
		Image: "app:v2", Scale: 1,
		Metadata: &types.ServiceMetadata{
			Generation:         5,
			TemplateGeneration: 5, // cast just stamped it
		},
	}
	instance := &types.Instance{
		ID: "svc-recast-abc", Name: "svc-recast-abc", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status:   types.InstanceStatusRunning,
		Metadata: &types.InstanceMetadata{ServiceGeneration: 1}, // old template
	}
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	compatible, reason := controller.IsInstanceCompatibleWithService(ctx, instance, service)
	assert.False(t, compatible, "a template change must recreate old-template instances")
	assert.Contains(t, reason, "service template changed")
}

// TestIsInstanceCompatible_PreMigrationServiceDoesNotBounce: services that
// predate TemplateGeneration have 0 there, while their instances recorded
// old-semantics Generation values (> 0). Nothing may bounce on upgrade — only
// the next real cast (which stamps TemplateGeneration) recreates.
func TestIsInstanceCompatible_PreMigrationServiceDoesNotBounce(t *testing.T) {
	ctx, _, testRunner, controller := setupTestController(t)

	service := &types.Service{
		ID: "svc-legacy", Name: "svc-legacy", Namespace: "default",
		Image: "app:v1", Scale: 1,
		Metadata: &types.ServiceMetadata{
			Generation:         13, // months of history
			TemplateGeneration: 0,  // pre-migration record
		},
	}
	instance := &types.Instance{
		ID: "svc-legacy-abc", Name: "svc-legacy-abc", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status:   types.InstanceStatusRunning,
		Metadata: &types.InstanceMetadata{ServiceGeneration: 13}, // old semantics
	}
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	compatible, reason := controller.IsInstanceCompatibleWithService(ctx, instance, service)
	assert.True(t, compatible,
		"pre-migration services must not bounce instances on upgrade; got reason: %s", reason)
}

// Mirror of the case above: the service has a template generation and the
// instance has none. It must read as outdated, or it keeps the old image with
// nothing in flight to replace it.
func TestIsInstanceCompatible_InstanceStampedZeroIsOutdated(t *testing.T) {
	ctx, _, testRunner, controller := setupTestController(t)

	service := &types.Service{
		ID: "svc-zeroed", Name: "svc-zeroed", Namespace: "default",
		Image: "app:v2", Scale: 1,
		Metadata: &types.ServiceMetadata{Generation: 1, TemplateGeneration: 1},
	}
	instance := &types.Instance{
		ID: "svc-zeroed-abc", Name: "svc-zeroed-abc", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status:   types.InstanceStatusRunning,
		Metadata: &types.InstanceMetadata{ServiceGeneration: 0},
	}
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	// Outdated specifically, not merely incompatible: Broken is deleted
	// immediately and outside the update budget, which is the whole-fleet
	// teardown the template counter exists to avoid.
	verdict := controller.ClassifyInstance(ctx, instance, service)
	assert.Equal(t, CompatOutdated, verdict.Class)
	assert.NotEmpty(t, verdict.Reason)
}

// TestIsInstanceCompatibleWithService_StuckInCreateHoldsSlot is the
// regression guard against the churn loop. A Failed record whose
// container never came up (ContainerEverCreatedAt == nil) must report
// as compatible so the reconciler does NOT tombstone+recreate-with-
// new-UUID every tick. The slot is held in place until an operator
// re-arms it via `rune restart <service>`.
func TestIsInstanceCompatibleWithService_StuckInCreateHoldsSlot(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	stuck := &types.Instance{
		ID:                     "stuck-id",
		Name:                   "stuck-0",
		Namespace:              "default",
		ServiceID:              service.ID,
		ServiceName:            service.Name,
		Status:                 types.InstanceStatusFailed,
		StatusMessage:          "failed to resolve volume mount",
		FailureReason:          "VolumeNotReady",
		ContainerEverCreatedAt: nil, // never had a container
		Metadata:               &types.InstanceMetadata{ServiceGeneration: 1},
	}

	ok, reason := controller.IsInstanceCompatibleWithService(ctx, stuck, service)
	assert.True(t, ok, "stuck-in-create record must claim its slot to break the churn loop")
	assert.Empty(t, reason)
}

// TestIsInstanceCompatibleWithService_VanishedContainerStillTriggersRecreate
// is the symmetrical regression guard: a record that WAS running
// (ContainerEverCreatedAt set) but whose container is gone from the
// runner (docker rm, host reboot, daemon crash) must still report
// incompatible so the existing tombstone+recreate path runs. Without
// this, real recovery scenarios silently stop working.
func TestIsInstanceCompatibleWithService_VanishedContainerStillTriggersRecreate(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	created := time.Now().Add(-1 * time.Hour)
	vanished := &types.Instance{
		ID:                     "vanished-id",
		Name:                   "vanished-0",
		Namespace:              "default",
		ServiceID:              service.ID,
		ServiceName:            service.Name,
		Runner:                 testRunner.Type(),
		Status:                 types.InstanceStatusRunning,
		ContainerEverCreatedAt: &created, // container did exist
		Metadata:               &types.InstanceMetadata{ServiceGeneration: 1},
	}
	// Container is gone: runner.Status will return error.
	testRunner.ErrorToReturn = assert.AnError

	ok, reason := controller.IsInstanceCompatibleWithService(ctx, vanished, service)
	assert.False(t, ok, "vanished-container records must trigger recreate so the workload recovers")
	assert.Contains(t, reason, "not found in runner")
}

// TestIsInstanceCompatibleWithService_StalledHoldsSlot mirrors the
// existing stuck-in-create gate but for the terminal Stalled state.
// Without this, a Stalled record would be tombstoned by the
// reconciler and we'd lose the operator-visible "intervention
// required" signal.
func TestIsInstanceCompatibleWithService_StalledHoldsSlot(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	stalled := &types.Instance{
		ID:                     "stalled-id",
		Name:                   "stalled-0",
		Namespace:              "default",
		ServiceID:              service.ID,
		ServiceName:            service.Name,
		Status:                 types.InstanceStatusStalled,
		ContainerEverCreatedAt: nil,
		Metadata:               &types.InstanceMetadata{ServiceGeneration: 1},
	}
	ok, reason := controller.IsInstanceCompatibleWithService(ctx, stalled, service)
	assert.True(t, ok, "Stalled stuck-in-create record must claim its slot")
	assert.Empty(t, reason)
}
