package types

import (
	"strings"
	"testing"
)

// Cross-mount conflict tests.
//
// ValidateMountPathConflicts is the single spot where mountPath
// collisions across volume / secret / configmap mounts are caught,
// where the system-path blocklist is enforced, and where the
// claim-at-scale-N foot-gun is rejected. Keep these table-driven so
// the rule-set stays scrutable.

func TestValidateMountPathConflicts_OK(t *testing.T) {
	err := ValidateMountPathConflicts("service \"x\"", 1,
		[]VolumeMount{
			{Name: "data", MountPath: "/var/data", Claim: &VolumeClaim{Name: "d"}},
		},
		[]SecretMount{
			{Name: "tls", MountPath: "/etc/tls", SecretName: "s"},
		},
		[]ConfigmapMount{
			{Name: "cfg", MountPath: "/etc/cfg", ConfigmapName: "c"},
		},
	)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateMountPathConflicts_CrossKindOverlap(t *testing.T) {
	cases := []struct {
		name string
		v    []VolumeMount
		s    []SecretMount
		c    []ConfigmapMount
		want string
	}{
		{
			name: "volume vs secret",
			v:    []VolumeMount{{Name: "d", MountPath: "/data", Claim: &VolumeClaim{Name: "d"}}},
			s:    []SecretMount{{Name: "tls", MountPath: "/data", SecretName: "s"}},
			want: "already used by volume mount",
		},
		{
			name: "secret vs configmap",
			s:    []SecretMount{{Name: "tls", MountPath: "/etc/x", SecretName: "s"}},
			c:    []ConfigmapMount{{Name: "cfg", MountPath: "/etc/x", ConfigmapName: "c"}},
			want: "already used by secret mount",
		},
		{
			name: "duplicate after path.Clean",
			v:    []VolumeMount{{Name: "d", MountPath: "/data", Claim: &VolumeClaim{Name: "d"}}},
			s:    []SecretMount{{Name: "tls", MountPath: "/data/", SecretName: "s"}},
			want: "already used by volume mount",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMountPathConflicts("service \"x\"", 1, tc.v, tc.s, tc.c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateMountPathConflicts_SystemBlocklist(t *testing.T) {
	cases := []string{"/proc", "/proc/self", "/sys", "/sys/kernel", "/dev", "/dev/pts", "/var/run/docker.sock", "/run/docker.sock"}
	for _, mp := range cases {
		t.Run(mp, func(t *testing.T) {
			err := ValidateMountPathConflicts("service \"x\"", 1,
				[]VolumeMount{{Name: "bad", MountPath: mp, Claim: &VolumeClaim{Name: "v"}}},
				nil, nil)
			if err == nil || !strings.Contains(err.Error(), "system blocklist") {
				t.Fatalf("want blocklist error for %s, got %v", mp, err)
			}
		})
	}
}

func TestValidateMountPathConflicts_RWOClaimAtScale(t *testing.T) {
	// `claim:` references a single existing volume; with scale > 1 the
	// controller can never satisfy it for ReadWriteOnce. claimTemplate
	// is the right tool — make sure we steer the user there.
	err := ValidateMountPathConflicts("service \"x\"", 3,
		[]VolumeMount{{Name: "data", MountPath: "/data", Claim: &VolumeClaim{Name: "shared"}}},
		nil, nil)
	if err == nil || !strings.Contains(err.Error(), "claimTemplate") {
		t.Fatalf("want claim-at-scale error pointing to claimTemplate, got %v", err)
	}

	// claimTemplate at scale > 1 is fine — each replica gets its own.
	err = ValidateMountPathConflicts("service \"x\"", 3,
		[]VolumeMount{{
			Name:      "data",
			MountPath: "/data",
			ClaimTemplate: &VolumeClaimTemplate{
				StorageClassName: "fast",
				Size:             "10Gi",
				AccessMode:       AccessModeRWO,
			},
		}},
		nil, nil)
	if err != nil {
		t.Fatalf("claimTemplate at scale=3 should be fine, got %v", err)
	}
}

// TestValidateProcessRuntimeVolumes covers the static cast-time check that
// prevents a runtime=process service from binding a non-local
// StorageClass via claimTemplate (RUNE-070). The whitelist is "",
// "local", and "local-host"; everything else is refused.
func TestValidateProcessRuntimeVolumes(t *testing.T) {
	mk := func(sc string) []VolumeMount {
		return []VolumeMount{{
			Name:      "data",
			MountPath: "/data",
			ClaimTemplate: &VolumeClaimTemplate{
				StorageClassName: sc,
				Size:             "1Gi",
				AccessMode:       AccessModeRWO,
			},
		}}
	}

	// Container runtime: rule does not apply.
	if err := ValidateProcessRuntimeVolumes("service \"x\"", RuntimeTypeContainer, mk("nfs-prod")); err != nil {
		t.Fatalf("container runtime should be unaffected, got %v", err)
	}

	// Process runtime + allowed classes.
	for _, sc := range []string{"", "local", "local-host"} {
		if err := ValidateProcessRuntimeVolumes("service \"x\"", RuntimeTypeProcess, mk(sc)); err != nil {
			t.Fatalf("process runtime + sc=%q should be allowed, got %v", sc, err)
		}
	}

	// Process runtime + disallowed class.
	err := ValidateProcessRuntimeVolumes("service \"x\"", RuntimeTypeProcess, mk("nfs-prod"))
	if err == nil || !strings.Contains(err.Error(), "runtime=process") {
		t.Fatalf("want runtime=process refusal, got %v", err)
	}

	// Claim references (vs. claimTemplate) are not subject to the
	// static check — they require a store lookup.
	err = ValidateProcessRuntimeVolumes("service \"x\"", RuntimeTypeProcess, []VolumeMount{{
		Name: "data", MountPath: "/data", Claim: &VolumeClaim{Name: "shared"},
	}})
	if err != nil {
		t.Fatalf("claim references should bypass the static check, got %v", err)
	}
}
