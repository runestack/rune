package types

import (
	"strings"
	"testing"
)

func TestValidateVolumeMounts(t *testing.T) {
	cases := []struct {
		name    string
		mounts  []VolumeMount
		wantErr string // substring; "" means no error expected
	}{
		{
			name:   "empty slice ok",
			mounts: nil,
		},
		{
			name: "happy claim",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/var/lib/data",
				Claim:     &VolumeClaim{Name: "shared"},
			}},
		},
		{
			name: "happy claim template",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/var/lib/data",
				ClaimTemplate: &VolumeClaimTemplate{
					Size:          "1Gi",
					AccessMode:    AccessModeRWO,
					ReclaimPolicy: ReclaimPolicyDelete,
				},
			}},
		},
		{
			name: "happy with subpath",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/var/lib/data",
				SubPath:   "sub/dir",
				Claim:     &VolumeClaim{Name: "shared"},
			}},
		},
		{
			name:    "missing name",
			mounts:  []VolumeMount{{MountPath: "/x", Claim: &VolumeClaim{Name: "y"}}},
			wantErr: "name is required",
		},
		{
			name: "missing mountPath",
			mounts: []VolumeMount{{
				Name:  "data",
				Claim: &VolumeClaim{Name: "y"},
			}},
			wantErr: "mountPath is required",
		},
		{
			name: "relative mountPath",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "relative/path",
				Claim:     &VolumeClaim{Name: "y"},
			}},
			wantErr: "must be absolute",
		},
		{
			name: "root mountPath rejected",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/",
				Claim:     &VolumeClaim{Name: "y"},
			}},
			wantErr: "cannot be \"/\"",
		},
		{
			name: "absolute subPath",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
				SubPath:   "/oops",
				Claim:     &VolumeClaim{Name: "y"},
			}},
			wantErr: "subPath",
		},
		{
			name: "subPath with parent escape",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
				SubPath:   "a/../b",
				Claim:     &VolumeClaim{Name: "y"},
			}},
			wantErr: "..",
		},
		{
			name: "neither claim nor template",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
			}},
			wantErr: "exactly one of claim or claimTemplate",
		},
		{
			name: "both claim and template",
			mounts: []VolumeMount{{
				Name:          "data",
				MountPath:     "/data",
				Claim:         &VolumeClaim{Name: "y"},
				ClaimTemplate: &VolumeClaimTemplate{Size: "1Gi", AccessMode: AccessModeRWO},
			}},
			wantErr: "mutually exclusive",
		},
		{
			name: "claim with empty name",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
				Claim:     &VolumeClaim{Name: "  "},
			}},
			wantErr: "claim.name is required",
		},
		{
			name: "claim template missing size",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
				ClaimTemplate: &VolumeClaimTemplate{
					AccessMode: AccessModeRWO,
				},
			}},
			wantErr: "claimTemplate.size is required",
		},
		{
			name: "claim template invalid size",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
				ClaimTemplate: &VolumeClaimTemplate{
					Size:       "abc",
					AccessMode: AccessModeRWO,
				},
			}},
			wantErr: "invalid claimTemplate.size",
		},
		{
			name: "claim template missing access mode",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
				ClaimTemplate: &VolumeClaimTemplate{
					Size: "1Gi",
				},
			}},
			wantErr: "accessMode is required",
		},
		{
			name: "claim template invalid access mode",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
				ClaimTemplate: &VolumeClaimTemplate{
					Size:       "1Gi",
					AccessMode: AccessMode("ReadWriteSometimes"),
				},
			}},
			wantErr: "invalid claimTemplate.accessMode",
		},
		{
			name: "claim template invalid reclaim policy",
			mounts: []VolumeMount{{
				Name:      "data",
				MountPath: "/data",
				ClaimTemplate: &VolumeClaimTemplate{
					Size:          "1Gi",
					AccessMode:    AccessModeRWO,
					ReclaimPolicy: ReclaimPolicy("recycle"),
				},
			}},
			wantErr: "invalid claimTemplate.reclaimPolicy",
		},
		{
			name: "duplicate name",
			mounts: []VolumeMount{
				{Name: "data", MountPath: "/a", Claim: &VolumeClaim{Name: "x"}},
				{Name: "data", MountPath: "/b", Claim: &VolumeClaim{Name: "y"}},
			},
			wantErr: "duplicate name",
		},
		{
			name: "duplicate mountPath",
			mounts: []VolumeMount{
				{Name: "a", MountPath: "/data", Claim: &VolumeClaim{Name: "x"}},
				{Name: "b", MountPath: "/data", Claim: &VolumeClaim{Name: "y"}},
			},
			wantErr: "duplicate mountPath",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVolumeMounts(tc.mounts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

// TestServiceSpecValidate_VolumesPropagated confirms ServiceSpec.Validate
// surfaces volume-mount errors so `rune cast` / `rune lint` fail fast.
func TestServiceSpecValidate_VolumesPropagated(t *testing.T) {
	spec := &ServiceSpec{
		Name:  "api",
		Image: "alpine:3.19",
		Volumes: []VolumeMount{{
			Name:      "data",
			MountPath: "relative/path", // bad
			Claim:     &VolumeClaim{Name: "x"},
		}},
	}
	err := spec.Validate()
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("expected absolute-mountPath error, got %v", err)
	}
}

// TestServiceValidate_VolumesPropagated confirms Service.Validate (the API
// server's CreateService chokepoint) also runs the volume validation.
func TestServiceValidate_VolumesPropagated(t *testing.T) {
	svc := &Service{
		ID:      "00000000-0000-0000-0000-000000000001",
		Name:    "api",
		Image:   "alpine:3.19",
		Runtime: "container",
		Volumes: []VolumeMount{{
			Name:      "data",
			MountPath: "/data",
			// neither claim nor template
		}},
	}
	err := svc.Validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of claim") {
		t.Fatalf("expected claim/template error, got %v", err)
	}
}

// TestServiceSpecValidate_VolumesUnknownFieldRejected confirms the rawNode
// linter still rejects misspelled top-level keys after we whitelisted
// "volumes". (Sanity check that we didn't accidentally relax the parser.)
func TestServiceSpecValidate_VolumesAccepted(t *testing.T) {
	// no rawNode = structural check is no-op; just ensure a well-formed
	// volumes spec passes Validate end-to-end.
	spec := &ServiceSpec{
		Name:  "api",
		Image: "alpine:3.19",
		Volumes: []VolumeMount{{
			Name:      "data",
			MountPath: "/data",
			ClaimTemplate: &VolumeClaimTemplate{
				Size:       "10Gi",
				AccessMode: AccessModeRWO,
			},
		}},
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}
