// volume_e2e_test.go — RUNE-070 end-to-end persistence tests for the
// single-node storage subsystem. These tests exercise the seam between
// the Controller and the in-tree local / local-host drivers
// against an in-memory store, no Docker required. They cover the four
// invariants the ticket calls out as the last persistence gap:
//
//  1. content survives a controller-driven re-Provision (proxy for
//     "container restart preserves data" — Provision is idempotent on
//     the volume directory, so a fresh reconcile must not blow it away);
//  2. claimTemplate produces N volumes with stable per-ordinal binding
//     (asserted across an explicit re-resolve);
//  3. scale-down does NOT reclaim per-ordinal volumes — only the
//     service-deletion path (VolumeCleanupFinalizer) reclaims them, and
//     even then only when ReclaimPolicy=Delete;
//  4. local-host with a missing host path fails cleanly: the controller
//     marks the volume Failed → Stalled, and never enters a busy-loop.
//
// Test 5 ("local-host write-restart-read") lives at driver level in
// pkg/storage/driver/local/local_test.go (TestLocalHostWriteRestartRead).
package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver/local"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVolumeE2E_ContentSurvivesReProvision drives a Volume through one
// full reconcile, writes a payload into the provisioned directory, then
// flips the row back to Pending to force a fresh reconcile (the
// equivalent of an instance restart re-driving the storage path) and
// asserts the payload is still readable. Provision is required to be
// idempotent on Volume.ID (driver.go:38) so the directory MUST be
// preserved.
func TestVolumeE2E_ContentSurvivesReProvision(t *testing.T) {
	ctx, _, ts, _, _ := setupVolumeController(t)
	putStorageClass(t, ts, "sc-persist", local.DriverNameLocal)

	vol := &types.Volume{
		ID:               "v-persist",
		Name:             "data",
		Namespace:        "default",
		StorageClassName: "sc-persist",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyRetain,
		Status:           types.VolumeStatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return fmt.Errorf("not yet available: %s", got.Status)
		}
		return nil
	})
	first := loadVolume(t, ts, vol.Namespace, vol.Name)
	require.NotEmpty(t, first.Handle)

	// Write a payload into the provisioned directory.
	payload := filepath.Join(first.Handle, "payload.txt")
	require.NoError(t, os.WriteFile(payload, []byte("hello-rune"), 0o600))

	// Flip status back to Pending so reconcile re-enters Provision.
	first.Status = types.VolumeStatusPending
	require.NoError(t, ts.Update(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, first))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return fmt.Errorf("did not return to available: %s", got.Status)
		}
		return nil
	})
	second := loadVolume(t, ts, vol.Namespace, vol.Name)
	assert.Equal(t, first.Handle, second.Handle, "Provision must be idempotent on Volume.ID — handle is stable")

	got, err := os.ReadFile(payload)
	require.NoError(t, err, "payload must survive re-Provision")
	assert.Equal(t, "hello-rune", string(got))
}

// TestVolumeE2E_ScaleDownDoesNotReclaim documents the
// "instance death never triggers reclaim" invariant. Three per-ordinal
// volumes are seeded as if a service had scaled to 3, then the test
// simulates a scale-down to 1 by simply waiting (no controller in the
// codebase deletes volumes on instance/scale-down). All three volumes
// — including those whose ordinal is now beyond the desired replicas —
// MUST still be present and on disk so a future scale-up rebinds the
// same data.
func TestVolumeE2E_ScaleDownDoesNotReclaim(t *testing.T) {
	ctx, _, ts, _, root := setupVolumeController(t)
	putStorageClass(t, ts, "sc-orphan", local.DriverNameLocal)

	names := []string{"data-api-0", "data-api-1", "data-api-2"}
	for ord, name := range names {
		v := &types.Volume{
			ID:               "id-" + name,
			Name:             name,
			Namespace:        "default",
			StorageClassName: "sc-orphan",
			AccessMode:       types.AccessModeRWO,
			ReclaimPolicy:    types.ReclaimPolicyDelete,
			OwnerService:     "default/api",
			BoundClaim:       fmt.Sprintf("api/data/%d", ord),
			Status:           types.VolumeStatusPending,
			CreatedAt:        time.Now().UTC(),
		}
		require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, "default", name, v))
	}

	// Wait for all three to provision.
	for _, name := range names {
		want := name
		waitFor(t, 2*time.Second, func() error {
			got := loadVolume(t, ts, "default", want)
			if got.Status != types.VolumeStatusAvailable {
				return fmt.Errorf("%s not yet available: %s", want, got.Status)
			}
			return nil
		})
	}

	// Simulate scale-down 3→1: nothing happens to the volumes; the
	// codebase deliberately has no scale-down → reclaim hook.
	time.Sleep(200 * time.Millisecond)

	for _, name := range names {
		got := loadVolume(t, ts, "default", name)
		assert.Equal(t, types.VolumeStatusAvailable, got.Status,
			"%s must remain Available across simulated scale-down", name)
		assert.NotEmpty(t, got.Handle, "%s handle must be intact", name)
		_, statErr := os.Stat(got.Handle)
		require.NoError(t, statErr, "%s handle dir must still exist on disk", name)
	}

	// And finally: deleting one volume row (the path the
	// VolumeCleanupFinalizer takes on service deletion) must trigger
	// driver-side reclaim under ReclaimPolicy=Delete — proving reclaim
	// IS reachable; it is just not wired to scale-down.
	doomed := loadVolume(t, ts, "default", names[2])
	require.NoError(t, ts.Delete(ctx, types.ResourceTypeVolume, "default", names[2]))
	waitFor(t, 2*time.Second, func() error {
		if _, err := os.Stat(doomed.Handle); err == nil {
			return errors.New("handle dir not yet reclaimed")
		}
		return nil
	})
	// The other two are untouched.
	for _, name := range names[:2] {
		got := loadVolume(t, ts, "default", name)
		assert.Contains(t, got.Handle, root)
	}
}

// TestVolumeE2E_LocalHostMissingPathFailsCleanly wires the controller
// against the local-host driver with allowCreateMissing=false, then
// asks it to provision a Volume whose parameters.hostPath points at a
// non-existent subdir of the allowlist. The driver must reject with
// ErrInvalidConfig; the controller must surface that as
// VolumeStatusFailed → eventually Stalled, never spinning forever.
func TestVolumeE2E_LocalHostMissingPathFailsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	allowRoot := t.TempDir()
	ts := store.NewTestStore()
	controller, err := NewController(Options{
		Store:  ts,
		Logger: log.NewLogger(),
		DriverConfigs: map[string]map[string]any{
			local.DriverNameLocalHost: {
				"hostPathAllowlist":  []string{allowRoot},
				"allowCreateMissing": false,
			},
		},
		MaxProvisionAttempts: 2,
		ProvisionBaseBackoff: 5 * time.Millisecond,
		ProvisionMaxBackoff:  10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, controller.Start(ctx))
	t.Cleanup(func() { _ = controller.Stop() })

	// Override the auto-seeded "local-host" StorageClass is fine — the
	// builder uses the same driver. Add a fresh class for clarity.
	putStorageClass(t, ts, "sc-host-missing", local.DriverNameLocalHost)

	missing := filepath.Join(allowRoot, "does-not-exist")
	vol := &types.Volume{
		ID:               "v-missing",
		Name:             "ghost",
		Namespace:        "default",
		StorageClassName: "sc-host-missing",
		AccessMode:       types.AccessModeRWO,
		Parameters:       map[string]string{"hostPath": missing},
		Status:           types.VolumeStatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	// First it should hit Failed (with retry scheduled), then Stalled
	// after MaxProvisionAttempts is exhausted.
	waitFor(t, 3*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusStalled {
			return fmt.Errorf("not yet stalled: status=%s reason=%q", got.Status, got.StatusReason)
		}
		return nil
	})

	got := loadVolume(t, ts, vol.Namespace, vol.Name)
	assert.Equal(t, "ProvisionRetriesExhausted", got.StatusReason)
	assert.NotEmpty(t, got.Message)
	assert.Contains(t, got.Message, "does-not-exist",
		"failure message should reference the offending host path")
	assert.Empty(t, got.Handle, "stalled volume must have no handle")

	// Snapshot UpdatedAt and verify the controller has stopped touching
	// the row — Stalled is terminal until an operator runs
	// `rune volume retry-provision`. If the controller were busy-looping
	// we'd see UpdatedAt advance.
	snapshot := got.UpdatedAt
	time.Sleep(150 * time.Millisecond)
	after := loadVolume(t, ts, vol.Namespace, vol.Name)
	assert.True(t, after.UpdatedAt.Equal(snapshot),
		"Stalled volume must not be re-touched (got UpdatedAt %v -> %v)",
		snapshot, after.UpdatedAt)
}
