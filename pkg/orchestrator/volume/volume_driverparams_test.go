package volume

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/storage/driver/local"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RUNE-200 PR 2: at successful Provision, the controller stamps the
// merged (class+volume) parameter map onto Volume.DriverParameters so
// reclaim / agent calls have something to work from if the class is
// deleted before its volumes. These tests cover both ends:
//
//   - the snapshot is captured on a successful Provision; and
//   - reclaimParameters() prefers the live class but falls back to the
//     snapshot when the class is gone, with volume-local Parameters
//     still layered on top.

func TestVolumeController_SnapshotsDriverParametersAtProvision(t *testing.T) {
	ctx, _, ts, _, _ := setupVolumeController(t)

	// StorageClass with two parameters (one of which the volume will override).
	class := &types.StorageClass{
		ID:     "sc-snap",
		Name:   "sc-snap",
		Driver: local.DriverNameLocal,
		Parameters: map[string]string{
			"region": "eu-west-1",
			"fsType": "ext4",
		},
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeStorageClass, "", class.Name, class))

	vol := &types.Volume{
		ID:               "v-snap",
		Name:             "with-overrides",
		Namespace:        "default",
		StorageClassName: "sc-snap",
		Size:             "1Mi",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		// Volume-local override for region; fsType inherits from class.
		Parameters: map[string]string{"region": "eu-west-2"},
		Status:     types.VolumeStatusPending,
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return errors.New("not yet available: " + string(got.Status))
		}
		if len(got.DriverParameters) == 0 {
			return errors.New("DriverParameters not yet stamped")
		}
		return nil
	})

	got := loadVolume(t, ts, vol.Namespace, vol.Name)

	// Snapshot must reflect the post-merge view: volume override wins
	// on `region`, class default carries through on `fsType`.
	assert.Equal(t, "eu-west-2", got.DriverParameters["region"],
		"volume Parameters must override class on the snapshot")
	assert.Equal(t, "ext4", got.DriverParameters["fsType"],
		"class Parameters carry through into the snapshot")

	// Mutating the live class after Provision must not affect the
	// snapshot — the controller stores a copy, not a reference.
	class.Parameters["fsType"] = "xfs"
	require.NoError(t, ts.Update(ctx, types.ResourceTypeStorageClass, "", class.Name, class))
	got = loadVolume(t, ts, vol.Namespace, vol.Name)
	assert.Equal(t, "ext4", got.DriverParameters["fsType"],
		"snapshot is point-in-time; later class edits must not bleed in")
}

func TestReclaimParameters_FallsBackToSnapshotWhenClassMissing(t *testing.T) {
	cases := []struct {
		name     string
		class    *types.StorageClass
		vol      *types.Volume
		want     map[string]string
		wantDesc string
	}{
		{
			name: "live class merges with volume parameters",
			class: &types.StorageClass{
				Parameters: map[string]string{"region": "eu-west-1", "fsType": "ext4"},
			},
			vol: &types.Volume{
				Parameters:       map[string]string{"region": "eu-west-2"},
				DriverParameters: map[string]string{"region": "stale", "fsType": "stale"},
			},
			// Live class wins over snapshot; volume Parameters still override class.
			want:     map[string]string{"region": "eu-west-2", "fsType": "ext4"},
			wantDesc: "live class beats snapshot",
		},
		{
			name:  "orphan class falls back to snapshot, then volume overrides",
			class: nil,
			vol: &types.Volume{
				Parameters:       map[string]string{"region": "eu-west-2"},
				DriverParameters: map[string]string{"region": "eu-west-1", "fsType": "ext4"},
			},
			want:     map[string]string{"region": "eu-west-2", "fsType": "ext4"},
			wantDesc: "snapshot used, volume Parameters layered on top",
		},
		{
			name:     "orphan class with no snapshot returns volume-only",
			class:    nil,
			vol:      &types.Volume{Parameters: map[string]string{"region": "eu-west-2"}},
			want:     map[string]string{"region": "eu-west-2"},
			wantDesc: "graceful fall-through to volume-only when neither source has class data",
		},
		{
			name:     "nil volume returns empty",
			class:    nil,
			vol:      nil,
			want:     map[string]string{},
			wantDesc: "no crash on nil volume",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reclaimParameters(tc.class, tc.vol)
			assert.Equal(t, tc.want, got, tc.wantDesc)
		})
	}
}

// Sanity: copyStringMap returns an independent map so a caller that
// stashes it doesn't see subsequent mutations on the source.
func TestCopyStringMap_IsIndependent(t *testing.T) {
	src := map[string]string{"a": "1", "b": "2"}
	cp := copyStringMap(src)
	src["a"] = "MUTATED"
	src["c"] = "added"
	assert.Equal(t, "1", cp["a"])
	assert.NotContains(t, cp, "c")

	// nil in → nil out.
	assert.Nil(t, copyStringMap(nil))
}

// Borrow the setupVolumeController helper from volume_controller_test.go;
// this is a sibling _test.go in the same package so it can reference
// unexported helpers directly. Keep an empty context.Background-style
// import so the package compiles even if no tests in this file call ctx.
var _ = context.Background
