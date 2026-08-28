package instance

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateInstance tests the CreateInstance method
func TestCreateInstance(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Test creating an instance for the service
	instance, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.NoError(t, err, "CreateInstance should not return an error")
	assert.NotNil(t, instance, "Instance should not be nil")
	assert.Equal(t, "test-instance-0", instance.Name, "Instance Name should match")
	assert.Equal(t, service.ID, instance.ServiceID, "Instance should reference the service")
	assert.Equal(t, types.InstanceStatusRunning, instance.Status, "Instance should be running")

	// Verify instance was stored
	storedInstance, err := testStore.GetInstanceByID(ctx, "default", instance.ID)
	require.NoError(t, err, "Instance should be in the store")
	assert.Equal(t, instance.ID, storedInstance.ID, "Stored instance ID should match")

	// Verify runner calls were made
	assert.Contains(t, testRunner.CreatedInstances, instance, "Runner should have created the instance")
	assert.Contains(t, testRunner.StartedInstances, instance.ID, "Runner should have started the instance")

	// Verify environment variables
	assert.Contains(t, instance.Environment, "RUNE_SERVICE_NAME", "Should have service name env var")
	assert.Contains(t, instance.Environment, "ENV_VAR1", "Should have service env vars")
	assert.Equal(t, "value1", instance.Environment["ENV_VAR1"], "Env var should have correct value")
}

// TestDeleteInstance tests the DeleteInstance method
func TestDeleteInstance(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service and instance
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store and runner
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Add to test runner's tracked instances
	err = testRunner.Create(ctx, instance)
	require.NoError(t, err, "Failed to add instance to runner")

	// Test deleting the instance
	err = controller.DeleteInstance(ctx, instance)
	require.NoError(t, err, "DeleteInstance should not return an error")

	// Verify instance status was updated
	storedInstance, err := testStore.GetInstanceByID(ctx, "default", "test-instance")
	require.NoError(t, err, "Instance should still be in store")
	assert.Equal(t, types.InstanceStatusDeleted, storedInstance.Status, "Instance status should be deleted")

	// Verify runner calls were made
	assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Runner should have stopped the instance")
	assert.Contains(t, testRunner.RemovedInstances, instance.ID, "Runner should have removed the instance")
}

// TestStopInstance tests the StopInstance method
func TestStopInstance(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service and instance
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store and runner
	instance := &types.Instance{
		ID:        "test-instance-stop",
		Name:      "test-instance-stop",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Add to test runner's tracked instances
	err = testRunner.Create(ctx, instance)
	require.NoError(t, err, "Failed to add instance to runner")

	// Test stopping the instance
	err = controller.StopInstance(ctx, instance)
	require.NoError(t, err, "StopInstance should not return an error")

	// Verify instance status was updated to stopped
	storedInstance, err := testStore.GetInstanceByID(ctx, "default", "test-instance-stop")
	require.NoError(t, err, "Instance should still be in store")
	assert.Equal(t, types.InstanceStatusStopped, storedInstance.Status, "Instance status should be stopped")
	assert.Equal(t, "Stopped by user", storedInstance.StatusMessage, "Status message should indicate stopped by user")

	// Verify runner stop call was made
	assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Runner should have stopped the instance")
}

// TestRestartInstance tests the RestartInstance method
func TestRestartInstance(t *testing.T) {
	// Create test objects
	testStore := store.NewTestStore()
	testRunner := runner.NewTestRunner()
	testRunnerMgr := manager.NewTestRunnerManager(nil)
	testRunnerMgr.SetDockerRunner(testRunner)
	testRunnerMgr.SetProcessRunner(testRunner)
	testLogger := log.NewLogger()

	// Create the instance controller
	controller := NewController(testStore, testRunnerMgr, testLogger)

	// Create test instances
	instance := &types.Instance{
		ID:          "test-instance",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service",
		ServiceName: "test-service",
		Status:      types.InstanceStatusRunning,
	}

	ctx := context.Background()

	t.Run("SkipsTombstonedService", func(t *testing.T) {
		// RFC #129 Phase 4: a service being torn down must never have an
		// instance resurrected under it — the reconcileDeletion cascade owns
		// those instances, and a health-triggered replacement here would
		// orphan a container the retired store-orphan sweep used to catch.
		st := store.NewTestStore()
		rm := manager.NewTestRunnerManager(nil)
		tr := runner.NewTestRunner()
		rm.SetDockerRunner(tr)
		rm.SetProcessRunner(tr)
		c := NewController(st, rm, log.NewLogger())

		now := time.Now()
		deleting := &types.Service{
			ID: "svc-del", Name: "svc-del", Namespace: "default", Runtime: "container",
			RestartPolicy: types.RestartPolicyAlways,
			Metadata:      &types.ServiceMetadata{DeletionTimestamp: &now},
		}
		require.NoError(t, st.CreateService(ctx, deleting))
		inst := &types.Instance{
			ID: "svc-del-0", Name: "svc-del-0", Namespace: "default",
			ServiceID: "svc-del", ServiceName: "svc-del", Status: types.InstanceStatusRunning,
		}
		require.NoError(t, st.CreateInstance(ctx, inst))

		err := c.RestartInstance(ctx, inst, RestartReasonHealthCheckFailure)
		require.NoError(t, err, "restart on a deleting service must be a quiet no-op")
		assert.Empty(t, tr.CreatedInstances, "no replacement instance may be created for a tombstoned service")
	})

	t.Run("RestartPolicy=Always", func(t *testing.T) {
		// Create test service with Always restart policy
		serviceAlways := &types.Service{
			ID:            "test-service",
			Name:          "test-service",
			Namespace:     "default",
			RestartPolicy: types.RestartPolicyAlways,
			Runtime:       "container",
		}

		// Add service to store
		err := testStore.CreateService(ctx, serviceAlways)
		assert.NoError(t, err)

		// Add instance to store
		err = testStore.CreateInstance(ctx, instance)
		assert.NoError(t, err)

		// Call the method
		err = controller.RestartInstance(context.Background(), instance, RestartReasonManual)

		// Verify: new tombstone+replace semantics. The original instance
		// is stopped (preserved), then a NEW replacement instance with the
		// same logical Name (but fresh UUID) is Create+Start'd.
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Original instance should have been stopped")
		require.NotEmpty(t, testRunner.CreatedInstances, "A replacement instance should have been created")
		assert.Equal(t, instance.Name, testRunner.CreatedInstances[len(testRunner.CreatedInstances)-1].Name,
			"Replacement should share the original's logical Name")
		assert.NotEqual(t, instance.ID, testRunner.CreatedInstances[len(testRunner.CreatedInstances)-1].ID,
			"Replacement should have a brand-new UUID")
		require.NotEmpty(t, testRunner.StartedInstances, "Replacement should have been started")
	})

	// Test with OnFailure restart policy
	t.Run("RestartPolicy=OnFailure", func(t *testing.T) {
		serviceOnFailure := &types.Service{
			ID:            "test-service",
			Name:          "test-service",
			Namespace:     "default",
			RestartPolicy: types.RestartPolicyOnFailure,
			Runtime:       "container",
		}

		// Reset between tests
		instance.Status = types.InstanceStatusRunning
		instance.FailedAt = nil
		instance.FailureReason = ""
		testStore.Reset()
		testRunner = runner.NewTestRunner() // Create a fresh runner
		testRunnerMgr = manager.NewTestRunnerManager(nil)
		testRunnerMgr.SetDockerRunner(testRunner)
		testRunnerMgr.SetProcessRunner(testRunner)
		controller = NewController(testStore, testRunnerMgr, testLogger)

		// Set up test data for OnFailure policy
		err := testStore.CreateService(ctx, serviceOnFailure)
		assert.NoError(t, err)

		err = testStore.CreateInstance(ctx, instance)
		assert.NoError(t, err)

		// Call with non-failure reason - should skip restart
		err = controller.RestartInstance(context.Background(), instance, RestartReasonUpdate)

		// Should not have any operations performed
		assert.NoError(t, err)
		assert.Empty(t, testRunner.StoppedInstances, "Instance should not have been stopped with update reason")
		assert.Empty(t, testRunner.CreatedInstances, "Instance should not have been created with update reason")
		assert.Empty(t, testRunner.StartedInstances, "Instance should not have been started with update reason")

		// Now call with failure reason - should restart.
		// New semantics: the original is tombstoned (Failed). Subsequent
		// RestartInstance calls against the same pointer no-op because
		// the store-loaded record reports Status=Failed (terminal state
		// short-circuit). The first failure call is the only one that
		// triggers Create/Start; re-build a fresh "live" instance to
		// re-exercise the path with different reasons.
		err = controller.RestartInstance(context.Background(), instance, RestartReasonFailure)
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Original should have been stopped")
		require.NotEmpty(t, testRunner.CreatedInstances, "Replacement should have been created")
		assert.Equal(t, instance.Name, testRunner.CreatedInstances[len(testRunner.CreatedInstances)-1].Name)
	})

	// Test with Never restart policy
	t.Run("RestartPolicy=Never", func(t *testing.T) {
		serviceOnNever := &types.Service{
			ID:            "test-service",
			Name:          "test-service",
			Namespace:     "default",
			RestartPolicy: types.RestartPolicyNever,
			Runtime:       "container",
		}

		// Reset between tests. The shared `instance` pointer may carry
		// Failed status mutated by a prior subtest's RestartInstance; reset
		// it so this block starts from a clean Running state.
		instance.Status = types.InstanceStatusRunning
		instance.FailedAt = nil
		instance.FailureReason = ""
		testStore.Reset()
		testRunner = runner.NewTestRunner() // Create a fresh runner
		testRunnerMgr = manager.NewTestRunnerManager(nil)
		testRunnerMgr.SetDockerRunner(testRunner)
		testRunnerMgr.SetProcessRunner(testRunner)
		controller = NewController(testStore, testRunnerMgr, testLogger)

		err := testStore.CreateService(ctx, serviceOnNever)
		assert.NoError(t, err)

		err = testStore.CreateInstance(ctx, instance)
		assert.NoError(t, err)

		// Call with automatic restart reasons - should not restart
		err = controller.RestartInstance(context.Background(), instance, RestartReasonFailure)
		assert.NoError(t, err)
		assert.Empty(t, testRunner.StoppedInstances, "Instance should not have been stopped with failure reason")
		assert.Empty(t, testRunner.CreatedInstances, "Instance should not have been created with failure reason")
		assert.Empty(t, testRunner.StartedInstances, "Instance should not have been started with failure reason")

		err = controller.RestartInstance(context.Background(), instance, RestartReasonHealthCheckFailure)
		assert.NoError(t, err)
		assert.Empty(t, testRunner.StoppedInstances, "Instance should not have been stopped with health check failure reason")
		assert.Empty(t, testRunner.CreatedInstances, "Instance should not have been created with health check failure reason")
		assert.Empty(t, testRunner.StartedInstances, "Instance should not have been started with health check failure reason")

		err = controller.RestartInstance(context.Background(), instance, RestartReasonUpdate)
		assert.NoError(t, err)
		assert.Empty(t, testRunner.StoppedInstances, "Instance should not have been stopped with update reason")
		assert.Empty(t, testRunner.CreatedInstances, "Instance should not have been created with update reason")
		assert.Empty(t, testRunner.StartedInstances, "Instance should not have been started with update reason")

		// Reset runner for next test
		testRunner = runner.NewTestRunner()
		testRunnerMgr.SetDockerRunner(testRunner)
		testRunnerMgr.SetProcessRunner(testRunner)

		// Call with manual restart - should restart even with Never policy.
		// New semantics: original tombstoned, fresh replacement created.
		err = controller.RestartInstance(context.Background(), instance, RestartReasonManual)
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Original should have been stopped (manual override)")
		require.NotEmpty(t, testRunner.CreatedInstances, "Replacement should have been created (manual override)")
		assert.Equal(t, instance.Name, testRunner.CreatedInstances[len(testRunner.CreatedInstances)-1].Name)
	})
}

// TestUpdateInstance tests the UpdateInstance method
func TestUpdateInstance(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store
	originalUpdateTime := time.Now().Add(-1 * time.Hour) // Old update time
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		Environment: map[string]string{
			"RUNE_SERVICE_NAME": "test-service",
			"ENV_VAR1":          "old-value", // This will be updated
		},
		Metadata: &types.InstanceMetadata{
			Image:             "original-image:latest",
			ServiceGeneration: 1, // Match the service generation
		},
		CreatedAt: time.Now(),
		UpdatedAt: originalUpdateTime,
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Set up test runner status
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	// Modify the service to test updating the instance
	service.Env["ENV_VAR1"] = "new-value"   // Changed value
	service.Env["ENV_VAR3"] = "added-value" // New env var

	// Test updating the instance
	err = controller.UpdateInstance(ctx, service, instance)
	require.NoError(t, err, "UpdateInstance should not return an error")

	// Verify instance was updated in store
	updatedInstance, err := testStore.GetInstanceByID(ctx, "default", instance.ID)
	require.NoError(t, err, "Instance should be in the store")

	// Check that the environment was updated correctly
	assert.Equal(t, "new-value", updatedInstance.Environment["ENV_VAR1"], "ENV_VAR1 should be updated")
	assert.Equal(t, "added-value", updatedInstance.Environment["ENV_VAR3"], "ENV_VAR3 should be added")

	// Check that updateAt time is newer
	assert.NotEqual(t, originalUpdateTime, updatedInstance.UpdatedAt, "UpdatedAt should be changed")

	// Separate test for incompatible update
	TestUpdateInstanceIncompatible(t)
}

// TestUpdateInstanceIncompatible pins UpdateInstance's contract for the two
// incompatibility classes (RUNE-042 §6.3).
//
// This test used to assert that ANY incompatibility — including a plain image
// change — made UpdateInstance return the "requires recreation" error. That is
// no longer right: an image change makes an instance OUTDATED, and an outdated
// instance is serving fine, so whether and when it gets replaced is the update
// budget's decision. UpdateInstance is called on every reconcile for every
// surviving instance, so erroring here would destroy outdated instances
// through this path regardless of any budget.
//
// System behaviour is unchanged in this phase: the reconciler's ensure* loops
// check compatibility themselves and delete outdated instances before ever
// reaching UpdateInstance (see ensureStatelessInstances), so the leave-alone
// branch below only absorbs the mid-reconcile race this used to recreate on.
// A BROKEN instance still errors, because repair is unbudgeted.
func TestUpdateInstanceIncompatible(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create service and instance that won't be compatible after changes
	service := &types.Service{
		ID:            "test-service-incompatible",
		Name:          "test-service-incompatible",
		Namespace:     "default",
		RestartPolicy: types.RestartPolicyAlways,
		Image:         "original-image:latest",
		Runtime:       "docker",
		Metadata: &types.ServiceMetadata{
			Generation: 1,
		},
	}

	err := testStore.CreateService(ctx, service)
	require.NoError(t, err, "Failed to create test service")

	// Create instance with original image
	instance := &types.Instance{
		ID:          "instance-to-recreate",
		Name:        "instance-to-recreate",
		Namespace:   "default",
		ServiceID:   service.ID,
		Status:      types.InstanceStatusRunning,
		ContainerID: "container123", // Important for docker runtime incompatibility check
		Metadata: &types.InstanceMetadata{
			Image: "original-image:latest",
		},
	}

	err = testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Set up test runner status
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	// Create a modified service with different image. An image change is a
	// TEMPLATE change, so cast stamps TemplateGeneration alongside Generation
	// (issue #142) — that's what triggers the incompatibility here.
	modifiedService := &types.Service{
		ID:            service.ID,
		Name:          service.Name,
		Namespace:     service.Namespace,
		RestartPolicy: service.RestartPolicy,
		Image:         "different-image:latest", // This should trigger incompatibility
		Runtime:       "docker",
		Metadata: &types.ServiceMetadata{
			Generation:         2,
			TemplateGeneration: 2,
		},
	}

	// An image change is a TEMPLATE change: the instance is OUTDATED, not
	// broken. UpdateInstance must leave it alone and let the update planner
	// decide when to replace it.
	err = controller.UpdateInstance(ctx, modifiedService, instance)
	assert.NoError(t, err,
		"an outdated instance is the update planner's business; UpdateInstance must not force its recreation")

	// A BROKEN instance is the case that still demands immediate recreation:
	// it is serving nobody, so there is nothing to budget.
	// ContainerEverCreatedAt matters: without it the record is "stuck in
	// create", which the classifier deliberately reports as OK so the
	// reconciler holds the slot instead of churning new UUIDs. A crashed
	// instance is one whose container DID exist.
	created := time.Now()
	broken := &types.Instance{
		ID:                     "instance-broken",
		Name:                   "instance-broken",
		Namespace:              "default",
		ServiceID:              service.ID,
		Status:                 types.InstanceStatusFailed,
		ContainerID:            "container456",
		ContainerEverCreatedAt: &created,
		Metadata:               &types.InstanceMetadata{Image: "original-image:latest"},
	}
	require.NoError(t, testStore.CreateInstance(ctx, broken))
	testRunner.StatusResults[broken.ID] = types.InstanceStatusFailed

	err = controller.UpdateInstance(ctx, modifiedService, broken)
	require.Error(t, err, "UpdateInstance must still surface the recreation signal for a broken instance")
	assert.Contains(t, err.Error(), "requires recreation", "Error should indicate recreation is needed")
}

// TestCreateInstance_SuccessSetsContainerEverCreatedAt asserts that a
// successful CreateInstance stamps ContainerEverCreatedAt and zeroes
// CreateAttempts. This is the load-bearing field used by the reconciler
// to tell "container vanished" apart from "create never succeeded";
// regressing this would re-introduce the churn loop documented in
// RUNE-BUG-RECONCILER-CHURN-ON-STABLE-PRECONDITION-FAILURE.
func TestCreateInstance_SuccessSetsContainerEverCreatedAt(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(t.Context(), t, controllerTestStore(controller), "test-service", types.RestartPolicyAlways)

	instance, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.NoError(t, err)
	require.NotNil(t, instance.ContainerEverCreatedAt, "ContainerEverCreatedAt must be set after first successful Create")
	assert.Equal(t, 0, instance.CreateAttempts, "CreateAttempts should be reset to 0 on success")
}

// TestRetryCreateInstance_ResetsTransientStateAndPreservesAttempts
// asserts the retry contract: Status flips back to Pending and
// NextCreateAttemptAt is cleared (a new attempt is happening), but
// CreateAttempts is preserved (the backoff schedule needs to know how
// many failures have already happened so the NEXT failure schedules
// the correct next-backoff and the Stalled threshold is reached
// after the right number of cumulative failures).
func TestRetryCreateInstance_ResetsTransientStateAndPreservesAttempts(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Seed a stuck-in-create record (as if 2 attempts had already failed).
	earlier := time.Now().Add(-1 * time.Minute)
	rec := &types.Instance{
		ID:                  "stuck-id",
		Name:                "stuck-0",
		Namespace:           "default",
		ServiceID:           service.ID,
		ServiceName:         service.Name,
		Status:              types.InstanceStatusFailed,
		StatusMessage:       "old error",
		FailureReason:       "VolumeNotReady",
		CreateAttempts:      2,
		NextCreateAttemptAt: &earlier,
		Metadata:            &types.InstanceMetadata{ServiceGeneration: 1},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))

	// Inject another failure so we can verify the post-retry state without success.
	testRunner.ErrorToReturn = assert.AnError
	err := controller.RetryCreateInstance(ctx, service, rec)
	require.Error(t, err, "retry must surface the new failure to the caller")

	var stored []types.Instance
	require.NoError(t, testStore.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1, "retry must not create a new UUID — same slot, same record")
	got := stored[0]
	assert.Equal(t, "stuck-id", got.ID, "UUID preserved")
	assert.Equal(t, 3, got.CreateAttempts, "CreateAttempts must increment cumulatively across retries")
	assert.NotNil(t, got.NextCreateAttemptAt, "next backoff scheduled")
	assert.NotEqual(t, "old error", got.StatusMessage, "StatusMessage refreshed to the new failure")
}

// TestRestartInstance_StuckInCreateReusesSameRecord asserts the
// operator-action path: `rune restart <service>` on a Stalled (or
// Failed-with-backoff) stuck-in-create record clears CreateAttempts
// and re-runs the create pipeline against the SAME UUID, instead of
// the tombstone+replace dance used for Running instances. Operators
// following an instance ID don't have to chase a moving identifier
// just because they restarted a stuck slot.
func TestRestartInstance_StuckInCreateReusesSameRecord(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Seed a Stalled stuck-in-create record (the case operators most
	// commonly encounter — retries exhausted, waiting for them to act).
	rec := &types.Instance{
		ID:             "stalled-id",
		Name:           "stalled-0",
		Namespace:      "default",
		ServiceID:      service.ID,
		ServiceName:    service.Name,
		Status:         types.InstanceStatusStalled,
		StatusMessage:  "old stalled reason",
		FailureReason:  "VolumeNotReady",
		CreateAttempts: maxCreateAttempts,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))

	require.NoError(t, controller.RestartInstance(ctx, rec, RestartReasonManual))

	var stored []types.Instance
	require.NoError(t, testStore.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1, "manual restart on stuck-in-create must NOT spawn a new UUID")
	got := stored[0]
	assert.Equal(t, "stalled-id", got.ID, "same UUID preserved across operator restart")
	// CreateAttempts was reset to 0 before the retry, then the (successful)
	// retry zeroes it again at the Running transition. Either way, must be 0.
	assert.Equal(t, 0, got.CreateAttempts, "operator restart resets attempt counter")
	assert.Equal(t, types.InstanceStatusRunning, got.Status, "successful retry promotes to Running")
}

// TestCreateInstance_WithReadinessProbe_StaysStartingUntilProbePasses
// asserts the readiness gate added in this PR: when a service
// defines a readiness probe, the freshly-created instance must NOT
// flip to Running on runner.Start success — it stays Starting until
// the health controller observes the first readiness pass. Without
// this, prod/gateway showed Status=Running for ~30s before the
// liveness probe killed it, even though the app was never ready to
// serve traffic.
func TestCreateInstance_WithReadinessProbe_StaysStartingUntilProbePasses(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)
	service.Health = &types.HealthCheck{
		Readiness: &types.Probe{Type: "http", Path: "/ready", Port: 8080},
	}
	// Persist the updated spec so prepareEnvVars / etc. see the same one.
	require.NoError(t, testStore.Update(ctx, types.ResourceTypeService, service.Namespace, service.Name, service))

	instance, err := controller.CreateInstance(ctx, service, "ready-gated-0", 0)
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusStarting, instance.Status,
		"with a readiness probe defined, runner.Start must NOT promote to Running")
	assert.Contains(t, instance.StatusMessage, "readiness probe",
		"status message must signal what we're waiting on")
}

// TestCreateInstance_NoReadinessProbe_PromotesToRunningAsBefore
// is the regression guard for the unchanged path: services WITHOUT
// a readiness probe still flip to Running on runner.Start (no
// probe = no signal = trust the runner). Tightening too far here
// would surprise every service that omitted readiness (i.e. most
// of them today).
func TestCreateInstance_NoReadinessProbe_PromotesToRunningAsBefore(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)
	// No service.Health → no readiness probe.

	instance, err := controller.CreateInstance(ctx, service, "no-probe-0", 0)
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusRunning, instance.Status)
}

// TestDeleteInstance_FlipsToTerminatingBeforeRunnerStop is the
// regression guard for the operator-visible UX gap reported live:
// after `rune delete service`, the instance kept showing Running
// for ~10s while runner.Stop's graceful-shutdown window elapsed.
// DeleteInstance must persist Status=Terminating BEFORE entering
// runner.Stop so `rune get instances` shows the truth immediately.
func TestDeleteInstance_FlipsToTerminatingBeforeRunnerStop(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	live := &types.Instance{
		ID:        "live-id",
		Name:      "live-0",
		Namespace: "default",
		Runner:    testRunner.Type(),
		Status:    types.InstanceStatusRunning,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", live.ID, live))

	// Intercept the runner.Stop call to inspect the store mid-teardown.
	// The store update to Terminating must have happened by the time
	// the runner sees the Stop call.
	var sawTerminating bool
	testRunner.StopFunc = func(ctx context.Context, instance *types.Instance, _ time.Duration) error {
		var current types.Instance
		require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", "live-id", &current))
		if current.Status == types.InstanceStatusTerminating {
			sawTerminating = true
		}
		return nil
	}

	require.NoError(t, controller.DeleteInstance(ctx, live))
	assert.True(t, sawTerminating,
		"Status=Terminating must be persisted BEFORE runner.Stop is invoked, not after")

	// And after the teardown completes, the final state is Deleted.
	var final types.Instance
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", "live-id", &final))
	assert.Equal(t, types.InstanceStatusDeleted, final.Status)
}

// TestDeleteInstance_DoesNotResurrectTerminalStateAsTerminating
// guards against the symmetrical hazard: if an instance is already
// Failed (a postmortem tombstone) or Stalled (retries exhausted),
// the in-place Terminating flip must NOT overwrite that. Otherwise
// `rune logs <tomb>` would lose the "Failed" signal mid-cleanup.
func TestDeleteInstance_DoesNotResurrectTerminalStateAsTerminating(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	at := time.Now()
	tomb := &types.Instance{
		ID:        "failed-id",
		Name:      "tomb-0",
		Namespace: "default",
		Runner:    testRunner.Type(),
		Status:    types.InstanceStatusFailed,
		FailedAt:  &at,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", tomb.ID, tomb))

	var seenStatusAtStop types.InstanceStatus
	testRunner.StopFunc = func(ctx context.Context, instance *types.Instance, _ time.Duration) error {
		var current types.Instance
		_ = testStore.Get(ctx, types.ResourceTypeInstance, "default", "failed-id", &current)
		seenStatusAtStop = current.Status
		return nil
	}

	require.NoError(t, controller.DeleteInstance(ctx, tomb))
	assert.Equal(t, types.InstanceStatusFailed, seenStatusAtStop,
		"Failed tombstones must NOT be transitioned to Terminating during DeleteInstance — they preserve postmortem state")
}
