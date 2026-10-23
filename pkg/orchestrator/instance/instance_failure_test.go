package instance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateInstance_RunnerCreateError_RecordsReason simulates a runner
// Create failure (the same shape as docker returning an error for an
// unresolvable image / volume / etc.) and asserts the instance record
// is updated in-place with the failure reason, NOT left at Pending
// with no detail. The user-facing payoff: `rune get instance -o yaml`
// now shows StatusMessage and FailureReason instead of an opaque
// Pending.
func TestCreateInstance_RunnerCreateError_RecordsReason(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Inject a runner-side create failure.
	testRunner.ErrorToReturn = assert.AnError
	_, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.Error(t, err)

	// The record must still exist (no leaking-blank-Pending), with
	// StatusMessage + FailureReason populated and ContainerEverCreatedAt
	// left nil so the reconciler treats it as stuck-in-create.
	var stored []types.Instance
	require.NoError(t, testStore.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1, "Failed create must leave the record in place")
	rec := stored[0]
	assert.Equal(t, types.InstanceStatusFailed, rec.Status)
	assert.NotEmpty(t, rec.StatusMessage, "StatusMessage must surface the failure to operators")
	assert.NotEmpty(t, rec.FailureReason, "FailureReason must be set so it can be filtered/searched")
	assert.Equal(t, 1, rec.CreateAttempts, "CreateAttempts must increment on failure")
	assert.Nil(t, rec.ContainerEverCreatedAt, "ContainerEverCreatedAt must remain nil when Create never succeeded")
	assert.Nil(t, rec.FailedAt, "FailedAt must remain nil — stuck-in-create is not a tombstone (would skew retention GC)")
}

// TestCreateBackoffFor_ExponentialWithCap verifies the schedule
// 30s → 1m → 2m → 4m → 5m(cap) — the contract referenced by the
// reconciler retry-in-place branch and documented in PR2's description.
// Tightening this without coordinating with the reconciler tick (30s)
// risks either retry-storms or extending the time-to-Stalled past
// what operators expect.
func TestCreateBackoffFor_ExponentialWithCap(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, 1 * time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 5 * time.Minute},  // capped
		{6, 5 * time.Minute},  // still capped
		{0, 30 * time.Second}, // clamps to attempt=1
	}
	for _, c := range cases {
		got := createBackoffFor(c.attempt)
		assert.Equal(t, c.want, got, "attempt %d", c.attempt)
	}
}

// TestRecordCreateFailure_SchedulesBackoff asserts that a non-terminal
// create failure populates NextCreateAttemptAt so the reconciler can
// honour the schedule. Without this, the reconciler would hit the
// retry path every tick and we'd be back to a 30s-cadence churn —
// just on the same UUID instead of new ones.
func TestRecordCreateFailure_SchedulesBackoff(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)
	testRunner.ErrorToReturn = assert.AnError

	before := time.Now()
	_, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.Error(t, err)

	var stored []types.Instance
	require.NoError(t, testStore.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1)
	rec := stored[0]
	require.NotNil(t, rec.NextCreateAttemptAt, "NextCreateAttemptAt must be scheduled after a failed attempt")
	delay := rec.NextCreateAttemptAt.Sub(before)
	assert.InDelta(t, (30 * time.Second).Seconds(), delay.Seconds(), 2.0,
		"first-attempt backoff should be ~30s, got %s", delay)
	assert.Equal(t, types.InstanceStatusFailed, rec.Status,
		"first failures stay at Failed; Stalled only after retries exhaust")
}

// TestRecordCreateFailure_StallsAfterMaxAttempts walks the record
// past maxCreateAttempts and asserts it flips to Stalled with
// NextCreateAttemptAt cleared (operators see a clear "stop waiting"
// signal). This is the contract operators rely on to know when
// manual intervention is required vs. when to keep watching.
func TestRecordCreateFailure_StallsAfterMaxAttempts(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	rec := &types.Instance{
		ID:        "stuck-id",
		Name:      "stuck-0",
		Namespace: "default",
		ServiceID: "test-service",
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))

	cc := controller
	// Walk attempts up to (max-1) — should still be Failed with backoff scheduled.
	for i := 0; i < maxCreateAttempts-1; i++ {
		cc.recordCreateFailure(ctx, rec, assert.AnError, "VolumeNotReady")
		assert.Equal(t, types.InstanceStatusFailed, rec.Status, "still Failed at attempt %d", rec.CreateAttempts)
		assert.NotNil(t, rec.NextCreateAttemptAt, "backoff scheduled at attempt %d", rec.CreateAttempts)
	}
	// The maxCreateAttempts-th failure flips to Stalled.
	cc.recordCreateFailure(ctx, rec, assert.AnError, "VolumeNotReady")
	assert.Equal(t, maxCreateAttempts, rec.CreateAttempts)
	assert.Equal(t, types.InstanceStatusStalled, rec.Status,
		"record must flip to Stalled after maxCreateAttempts failures")
	assert.Nil(t, rec.NextCreateAttemptAt,
		"Stalled records must NOT schedule auto-retry — operator must restart")
}

// TestDeleteInstance_SnapshotsLastLogsBeforeTearDown is the
// regression guard that closes the loop on bug
// RUNE-BUG-RUNE-LOGS-IGNORES-TOMBSTONE-LASTLOGS: the LastLogs field
// existed in the type but was never populated, so the
// service-level tombstone fallback (GetServiceLogs → most-recent
// tombstone with LastLogs) had nothing to read. DeleteInstance is
// the lifecycle moment that destroys the container; if we don't
// snapshot here, the only postmortem trail (LastLogs) goes with it.
func TestDeleteInstance_SnapshotsLastLogsBeforeTearDown(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	created := time.Now().Add(-1 * time.Hour)
	rec := &types.Instance{
		ID:                     "with-container",
		Name:                   "doomed-0",
		Namespace:              "default",
		Runner:                 testRunner.Type(),
		Status:                 types.InstanceStatusRunning,
		ContainerEverCreatedAt: &created, // had a container
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))
	testRunner.LogOutput = []byte("captured stderr from the dying container\n")

	require.NoError(t, controller.DeleteInstance(ctx, rec))

	var stored types.Instance
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", rec.ID, &stored))
	assert.Equal(t, types.InstanceStatusDeleted, stored.Status)
	assert.NotEmpty(t, stored.LastLogs, "DeleteInstance must snapshot LastLogs before tearing the container down")
	assert.Equal(t, "captured stderr from the dying container\n", string(stored.LastLogs))
	require.NotNil(t, stored.LastLogsCapturedAt)
}

// TestSnapshotInstanceLogs_NoOpForNeverCreated guards the
// optimisation that skips snapshotting when there was no container
// in the first place (precondition-failed records). Without this
// the snapshot would invoke the runner with no container to look
// at, generating noise.
func TestSnapshotInstanceLogs_NoOpForNeverCreated(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	rec := &types.Instance{
		ID:                     "never-had-container",
		Name:                   "stuck-0",
		Namespace:              "default",
		Runner:                 testRunner.Type(),
		Status:                 types.InstanceStatusFailed,
		ContainerEverCreatedAt: nil,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))
	testRunner.LogOutput = []byte("should not be read")

	cc := controller
	cc.snapshotInstanceLogs(ctx, rec)

	assert.Empty(t, rec.LastLogs, "stuck-in-create records (no container) must not trigger a runner.GetLogs call")
}

// startFailingRunner creates a container (with an ID) on every Create and
// fails every Start, tracking which containers still exist.
type startFailingRunner struct {
	*runner.TestRunner
	created   int
	live      map[string]bool
	running   map[string]bool
	removeErr error
	removed   []string
}

func newStartFailingRunner() *startFailingRunner {
	return &startFailingRunner{TestRunner: runner.NewTestRunner(), live: map[string]bool{}, running: map[string]bool{}}
}

func (r *startFailingRunner) Create(_ context.Context, instance *types.Instance) error {
	r.created++
	instance.ContainerID = fmt.Sprintf("container-%d", r.created)
	r.live[instance.ContainerID] = true
	return nil
}

func (r *startFailingRunner) Start(context.Context, *types.Instance) error {
	return assert.AnError
}

func (r *startFailingRunner) Status(_ context.Context, instance *types.Instance) (types.InstanceStatus, error) {
	if !r.live[instance.ContainerID] {
		return "", errdefs.ErrNotFound
	}
	if r.running[instance.ContainerID] {
		return types.InstanceStatusRunning, nil
	}
	return types.InstanceStatusFailed, nil
}

func (r *startFailingRunner) Remove(_ context.Context, instance *types.Instance, force bool) error {
	if r.removeErr != nil {
		return r.removeErr
	}
	if !r.live[instance.ContainerID] {
		return fmt.Errorf("failed to remove container: %w", errdefs.ErrNotFound)
	}
	if r.running[instance.ContainerID] && !force {
		return fmt.Errorf("cannot remove running container")
	}
	r.removed = append(r.removed, instance.ContainerID)
	delete(r.live, instance.ContainerID)
	return nil
}

func setupStartFailing(t *testing.T) (context.Context, *store.TestStore, *Controller, *types.Service, *startFailingRunner) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)
	r := newStartFailingRunner()
	mgr := manager.NewTestRunnerManager(nil)
	mgr.SetDockerRunner(r)
	mgr.SetProcessRunner(r)
	controller.runnerManager = mgr

	_, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.Error(t, err)
	return ctx, testStore, controller, service, r
}

func storedOnly(ctx context.Context, t *testing.T, s *store.TestStore) types.Instance {
	var stored []types.Instance
	require.NoError(t, s.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1)
	return stored[0]
}

// A Start failure must not lose the container ID: retries would otherwise
// create one more container per attempt for the same instance.
func TestRetryCreateInstance_StartFailureDoesNotAccumulateContainers(t *testing.T) {
	ctx, testStore, controller, service, r := setupStartFailing(t)

	for attempt := 1; attempt <= 3; attempt++ {
		rec := storedOnly(ctx, t, testStore)
		assert.Equal(t, fmt.Sprintf("container-%d", attempt), rec.ContainerID, "attempt %d: container ID persisted", attempt)
		assert.Nil(t, rec.ContainerEverCreatedAt, "attempt %d: must stay stuck-in-create, not broken", attempt)
		assert.Len(t, r.live, 1, "attempt %d: one container per instance", attempt)

		require.Error(t, controller.RetryCreateInstance(ctx, service, &rec))
	}
	assert.Equal(t, []string{"container-1", "container-2", "container-3"}, r.removed)
}

// Only the recorded container is removed, even when another container
// carries the same instance.
func TestRetryCreateInstance_RemovesOnlyRecordedContainer(t *testing.T) {
	ctx, testStore, controller, service, r := setupStartFailing(t)
	r.live["unrelated"] = true

	rec := storedOnly(ctx, t, testStore)
	require.Error(t, controller.RetryCreateInstance(ctx, service, &rec))
	assert.Equal(t, []string{"container-1"}, r.removed)
	assert.True(t, r.live["unrelated"])
}

func TestRetryCreateInstance_RecordedContainerAlreadyGone(t *testing.T) {
	ctx, testStore, controller, service, r := setupStartFailing(t)
	delete(r.live, "container-1")

	rec := storedOnly(ctx, t, testStore)
	require.Error(t, controller.RetryCreateInstance(ctx, service, &rec))
	assert.Equal(t, 2, r.created, "not-found counts as removed, so the retry creates")
	assert.Equal(t, "container-2", storedOnly(ctx, t, testStore).ContainerID)
}

func TestRetryCreateInstance_RecordedContainerRunningIsKept(t *testing.T) {
	ctx, testStore, controller, service, r := setupStartFailing(t)
	r.running["container-1"] = true

	rec := storedOnly(ctx, t, testStore)
	require.Error(t, controller.RetryCreateInstance(ctx, service, &rec))
	assert.Empty(t, r.removed)
	assert.Equal(t, 1, r.created, "no new container while the old one runs")
	got := storedOnly(ctx, t, testStore)
	assert.Equal(t, "container-1", got.ContainerID)
	assert.Equal(t, "PreviousContainerRunning", got.FailureReason)
	assert.Contains(t, got.StatusMessage, "container-1")
}

func TestRetryCreateInstance_RemoveFailureKeepsRecordedID(t *testing.T) {
	ctx, testStore, controller, service, r := setupStartFailing(t)
	r.removeErr = assert.AnError

	rec := storedOnly(ctx, t, testStore)
	require.Error(t, controller.RetryCreateInstance(ctx, service, &rec))
	assert.Equal(t, 1, r.created, "no new container until the old one is removed")
	got := storedOnly(ctx, t, testStore)
	assert.Equal(t, "container-1", got.ContainerID)
	assert.Equal(t, "PreviousContainerRemoveFailed", got.FailureReason)
	assert.Equal(t, 2, got.CreateAttempts)

	r.removeErr = nil
	require.Error(t, controller.RetryCreateInstance(ctx, service, &got))
	assert.Equal(t, []string{"container-1"}, r.removed, "the next retry removes it")
	assert.Equal(t, "container-2", storedOnly(ctx, t, testStore).ContainerID)
}
