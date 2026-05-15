package controllers

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/storage/driver/local"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bug F follow-up (RUNE storage handoff): volumeSizeBytes is the
// helper that parses Volume.Size into the int64 byte count drivers
// expect on Provision. Pre-v0.0.1-dev.46 the controller hardcoded
// SizeBytes=0, so do-volume rejected every provision with
// "invalid size 0 bytes" even though the spec round-tripped a valid
// Size string. These cases lock the conversion in.
func TestVolumeSizeBytes(t *testing.T) {
	cases := []struct {
		name string
		size string
		want int64
	}{
		// Empty Size is allowed for drivers that don't need it
		// (local, local-host bind-mounts) — the controller must not
		// reject this; per-driver validation handles "I needed a
		// size and you didn't give me one".
		{"empty", "", 0},

		// Kubernetes Quantity binary units.
		{"1Ki", "1Ki", 1024},
		{"1Mi", "1Mi", 1024 * 1024},
		{"1Gi", "1Gi", 1024 * 1024 * 1024},
		{"10Gi", "10Gi", 10 * 1024 * 1024 * 1024},
		{"1024Mi == 1Gi", "1024Mi", 1024 * 1024 * 1024},

		// SI decimal units (DigitalOcean's API uses these).
		{"1G", "1G", 1_000_000_000},
		{"500M", "500M", 500_000_000},

		// Bare integers are bytes (Quantity spec). The docs guide
		// previously used `"10"` for what an operator would
		// reasonably read as "10 gigabytes" — that's now documented
		// explicitly in the storage-resources reference as bytes,
		// and the guide example uses "10Gi" instead.
		{"bare 1073741824 == 1Gi", "1073741824", 1_073_741_824},
		{"bare 1", "1", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := volumeSizeBytes(&types.Volume{Size: tc.size})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestVolumeSizeBytes_NilVolume(t *testing.T) {
	got, err := volumeSizeBytes(nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got)
}

func TestVolumeSizeBytes_RejectsUnparseable(t *testing.T) {
	cases := []string{
		"banana",      // not a number
		"1.5.0",       // not parseable as float
		"1XB",         // unknown unit
		"1ZebraBytes", // gibberish unit
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			got, err := volumeSizeBytes(&types.Volume{
				Namespace: "default",
				Name:      "broken",
				Size:      s,
			})
			require.Error(t, err)
			assert.Equal(t, int64(0), got)
			// The error must name the offending volume and the
			// offending string so the controller log identifies
			// which Volume is wedged.
			assert.Contains(t, err.Error(), "default/broken")
			assert.Contains(t, err.Error(), s)
		})
	}
}

// Integration regression for the Propeller report: a volume with a
// well-formed Size must reach Available (locks in that the controller
// is actually passing a non-zero SizeBytes to drivers; the local
// driver doesn't validate the count but it's the same code path
// dovolume hits).
func TestVolumeController_PropagatesSizeToDriver(t *testing.T) {
	ctx, _, ts, _, _ := setupVolumeController(t)
	putStorageClass(t, ts, "sc-size", local.DriverNameLocal)

	vol := &types.Volume{
		ID:               "v-size",
		Name:             "sized",
		Namespace:        "default",
		StorageClassName: "sc-size",
		Size:             "1Gi",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		Status:           types.VolumeStatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return errors.New("not available: " + string(got.Status))
		}
		return nil
	})
}

// An unparseable Size is a spec error, not a driver failure — the
// controller should fail it terminally rather than burn retry budget.
func TestVolumeController_FailsTerminallyOnInvalidSize(t *testing.T) {
	ctx, _, ts, _, _ := setupVolumeController(t)
	putStorageClass(t, ts, "sc-bad", local.DriverNameLocal)

	vol := &types.Volume{
		ID:               "v-bad",
		Name:             "junk-size",
		Namespace:        "default",
		StorageClassName: "sc-bad",
		Size:             "banana",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		Status:           types.VolumeStatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusFailed {
			return errors.New("not Failed yet: " + string(got.Status))
		}
		if got.Message == "" {
			return errors.New("Message not set")
		}
		return nil
	})

	got := loadVolume(t, ts, vol.Namespace, vol.Name)
	if !strings.Contains(strings.ToLower(got.Message), "invalid size") &&
		!strings.Contains(strings.ToLower(got.Message), "banana") {
		t.Errorf("Message should mention the bad size; got %q", got.Message)
	}
}

// Drivers that don't need a size (local) must keep working with
// an empty Size — empty is a valid input, not an error.
func TestVolumeController_EmptySizeIsValid(t *testing.T) {
	ctx, _, ts, _, _ := setupVolumeController(t)
	putStorageClass(t, ts, "sc-empty", local.DriverNameLocal)

	vol := &types.Volume{
		ID:               "v-empty",
		Name:             "no-size",
		Namespace:        "default",
		StorageClassName: "sc-empty",
		Size:             "",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		Status:           types.VolumeStatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return errors.New("not available: " + string(got.Status))
		}
		return nil
	})
}
