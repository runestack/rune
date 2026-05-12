package service

import (
	"context"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver/local"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// newVolumeTestServiceWithClasses returns a VolumeService whose
// underlying store has the requested StorageClasses pre-seeded, plus
// the optional driver-config map for runefile-style lint hooks.
func newVolumeTestServiceWithClasses(t *testing.T, classes []*types.StorageClass, driverConfigs map[string]map[string]any) (*VolumeService, store.Store) {
	t.Helper()
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		ConfigLimits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
	scRepo := repos.NewStorageClassRepo(st)
	for _, sc := range classes {
		if err := scRepo.Create(context.Background(), sc); err != nil {
			t.Fatalf("seed storage class %q: %v", sc.Name, err)
		}
	}
	svc := NewVolumeService(st, log.GetDefaultLogger(), WithDriverConfigs(driverConfigs))
	return svc, st
}

// TestVolumeServiceLintAccessMode covers the cast-time check that
// rejects volumes whose AccessMode is not in the bound driver's
// Capabilities.AccessModes.
func TestVolumeServiceLintAccessMode(t *testing.T) {
	ctx := context.Background()
	svc, _ := newVolumeTestServiceWithClasses(t, []*types.StorageClass{
		{Name: "local", Driver: "local"},
	}, nil)

	// "local" driver Capabilities only declares ReadWriteOnce.
	_, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name:             "data",
			Namespace:        "ns",
			StorageClassName: "local",
			Size:             "1Gi",
			AccessMode:       "ReadWriteMany",
		},
		EnsureNamespace: true,
	})
	if err == nil {
		t.Fatalf("expected lint error for unsupported access mode")
	}
	if !strings.Contains(err.Error(), "accessMode") || !strings.Contains(err.Error(), "ReadWriteMany") {
		t.Fatalf("expected error to mention accessMode + ReadWriteMany, got %v", err)
	}

	// Supported access mode passes lint.
	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name:             "data-ok",
			Namespace:        "ns",
			StorageClassName: "local",
			Size:             "1Gi",
			AccessMode:       "ReadWriteOnce",
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("expected supported access mode to pass lint, got %v", err)
	}
}

// TestVolumeServiceLintLocalHostHostPath covers the cast-time check
// that local-host volumes must declare a parameters.hostPath inside
// the runefile's hostPathAllowlist.
func TestVolumeServiceLintLocalHostHostPath(t *testing.T) {
	ctx := context.Background()
	driverCfg := map[string]map[string]any{
		local.DriverNameLocalHost: {
			"hostPathAllowlist": []any{"/mnt/volumes"},
		},
	}
	svc, _ := newVolumeTestServiceWithClasses(t, []*types.StorageClass{
		{Name: "host", Driver: local.DriverNameLocalHost},
	}, driverCfg)

	cases := []struct {
		name       string
		params     map[string]string
		wantErrSub string
	}{
		{
			name:       "missing hostPath",
			params:     nil,
			wantErrSub: "parameters.hostPath",
		},
		{
			name:       "outside allowlist",
			params:     map[string]string{"hostPath": "/etc/shadow"},
			wantErrSub: "hostPathAllowlist",
		},
		{
			name:   "inside allowlist",
			params: map[string]string{"hostPath": "/mnt/volumes/flo-data"},
		},
		{
			name:   "exactly the allowlist root",
			params: map[string]string{"hostPath": "/mnt/volumes"},
		},
		{
			name:       "sibling-dir prefix attack",
			params:     map[string]string{"hostPath": "/mnt/volumes2/data"},
			wantErrSub: "hostPathAllowlist",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
				Volume: &generated.Volume{
					Name:             vName(i),
					Namespace:        "ns",
					StorageClassName: "host",
					Size:             "1Gi",
					AccessMode:       "ReadWriteOnce",
					Parameters:       tc.params,
				},
				EnsureNamespace: true,
			})
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("expected lint to pass, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected lint error containing %q, got nil", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("expected error to contain %q, got %v", tc.wantErrSub, err)
			}
		})
	}
}

// TestVolumeServiceLintMergesClassParameters confirms the local-host
// hostPath check merges StorageClass.Parameters with Volume.Parameters
// (volume overrides class) before applying the allowlist.
func TestVolumeServiceLintMergesClassParameters(t *testing.T) {
	ctx := context.Background()
	driverCfg := map[string]map[string]any{
		local.DriverNameLocalHost: {
			"hostPathAllowlist": []any{"/mnt/volumes"},
		},
	}
	svc, _ := newVolumeTestServiceWithClasses(t, []*types.StorageClass{
		// Class supplies a default hostPath inside the allowlist.
		{Name: "host", Driver: local.DriverNameLocalHost,
			Parameters: map[string]string{"hostPath": "/mnt/volumes/default"}},
	}, driverCfg)

	// No per-volume override → inherits class default → passes.
	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name: "inherits", Namespace: "ns", StorageClassName: "host",
			Size: "1Gi", AccessMode: "ReadWriteOnce",
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("expected inherited hostPath to pass: %v", err)
	}

	// Per-volume override outside allowlist → rejected.
	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name: "overrides", Namespace: "ns", StorageClassName: "host",
			Size: "1Gi", AccessMode: "ReadWriteOnce",
			Parameters: map[string]string{"hostPath": "/tmp/escape"},
		},
		EnsureNamespace: true,
	}); err == nil {
		t.Fatalf("expected per-volume override outside allowlist to fail")
	}
}

// TestVolumeServiceLintDefersOnMissingClass confirms that creates
// referencing an unseeded StorageClass still go through (deferred to
// the controller) rather than being rejected by the lint.
func TestVolumeServiceLintDefersOnMissingClass(t *testing.T) {
	ctx := context.Background()
	svc, _ := newVolumeTestServiceWithClasses(t, nil, nil)

	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name: "x", Namespace: "ns", StorageClassName: "nonexistent",
			Size: "1Gi", AccessMode: "ReadWriteMany", // would fail caps lint if class existed
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("expected lint to defer on missing class, got %v", err)
	}
}

// TestVolumeServiceLintDefersOnUnknownDriver confirms that creates
// referencing a StorageClass whose driver is not registered defer to
// the controller (so an out-of-tree driver image can be deployed
// without breaking the API server).
func TestVolumeServiceLintDefersOnUnknownDriver(t *testing.T) {
	ctx := context.Background()
	svc, _ := newVolumeTestServiceWithClasses(t, []*types.StorageClass{
		{Name: "exotic", Driver: "not-registered"},
	}, nil)

	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name: "x", Namespace: "ns", StorageClassName: "exotic",
			Size: "1Gi", AccessMode: "ReadWriteMany",
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("expected lint to defer on unknown driver, got %v", err)
	}
}

// TestVolumeServiceLintAppliesOnUpdate confirms that UpdateVolume runs
// the same lint as CreateVolume (an operator can't sneak around the
// check by creating-then-updating).
func TestVolumeServiceLintAppliesOnUpdate(t *testing.T) {
	ctx := context.Background()
	svc, _ := newVolumeTestServiceWithClasses(t, []*types.StorageClass{
		{Name: "local", Driver: "local"},
	}, nil)

	if _, err := svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume: &generated.Volume{
			Name: "data", Namespace: "ns", StorageClassName: "local",
			Size: "1Gi", AccessMode: "ReadWriteOnce",
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := svc.UpdateVolume(ctx, &generated.UpdateVolumeRequest{
		Volume: &generated.Volume{
			Name: "data", Namespace: "ns", StorageClassName: "local",
			Size: "1Gi", AccessMode: "ReadWriteMany", // unsupported
		},
	}); err == nil {
		t.Fatalf("expected update to fail capability lint")
	}
}

func vName(i int) string {
	return "v" + string(rune('a'+i))
}
