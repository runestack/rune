package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withVolume returns a copy of the base service with a single
// claim-template volume named "data" mounted at /data.
func withVolume(t *testing.T) *Service {
	t.Helper()
	s := makeBaseService()
	s.Volumes = []VolumeMount{{
		Name:      "data",
		MountPath: "/data",
		ClaimTemplate: &VolumeClaimTemplate{
			StorageClassName: "local-host",
			Size:             "1Gi",
			AccessMode:       AccessModeRWO,
		},
	}}
	return s
}

func TestServiceValidate_InitSteps_HappyPath_FreshVolume(t *testing.T) {
	s := withVolume(t)
	s.InitSteps = []InitStep{{
		Name:    "format",
		Image:   "repo/tool:1",
		Command: "/bin/format",
		Args:    []string{"--cluster=0", "/data/0_0.db"},
		// Volumes nil → inherit all parent volumes; runIf default freshVolume.
	}}
	require.NoError(t, s.Validate())
}

func TestServiceValidate_InitSteps_HappyPath_FileMissing(t *testing.T) {
	s := withVolume(t)
	s.InitSteps = []InitStep{{
		Name:    "format",
		Image:   "repo/tool:1",
		Command: "/bin/format",
		Volumes: []string{"data"},
		RunIf:   RunIf{Type: RunIfFileMissing, Path: "/data/0_0.db"},
	}}
	require.NoError(t, s.Validate())
}

func TestServiceValidate_InitSteps_HappyPath_Always(t *testing.T) {
	s := makeBaseService()
	s.InitSteps = []InitStep{{
		Name:    "warmup",
		Image:   "repo/tool:1",
		Command: "/bin/warmup",
		Volumes: []string{}, // mount none, allowed for always
		RunIf:   RunIf{Type: RunIfAlways},
	}}
	require.NoError(t, s.Validate())
}

func TestServiceValidate_InitSteps_RejectsBadInputs(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Service)
		wantSub string
	}{
		{
			name: "missing name",
			mutate: func(s *Service) {
				s.InitSteps = []InitStep{{Image: "i", Command: "c"}}
			},
			wantSub: "name is required",
		},
		{
			name: "invalid name",
			mutate: func(s *Service) {
				s.InitSteps = []InitStep{{Name: "Bad_Name", Image: "i", Command: "c"}}
			},
			wantSub: "DNS-1123",
		},
		{
			name: "duplicate name",
			mutate: func(s *Service) {
				s.InitSteps = []InitStep{
					{Name: "x", Image: "i", Command: "c", RunIf: RunIf{Type: RunIfAlways}},
					{Name: "x", Image: "i", Command: "c", RunIf: RunIf{Type: RunIfAlways}},
				}
			},
			wantSub: "duplicate step name",
		},
		{
			name: "container runtime missing image",
			mutate: func(s *Service) {
				s.InitSteps = []InitStep{{Name: "x", Command: "c", RunIf: RunIf{Type: RunIfAlways}}}
			},
			wantSub: "image is required",
		},
		{
			name: "missing command",
			mutate: func(s *Service) {
				s.InitSteps = []InitStep{{Name: "x", Image: "i", RunIf: RunIf{Type: RunIfAlways}}}
			},
			wantSub: "command is required",
		},
		{
			name: "volume filter references unknown parent volume",
			mutate: func(s *Service) {
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					Volumes: []string{"nope"},
					RunIf:   RunIf{Type: RunIfAlways},
				}}
			},
			wantSub: "does not match any parent service volume",
		},
		{
			name: "fileMissing without path",
			mutate: func(s *Service) {
				*s = *withVolume(t)
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					RunIf: RunIf{Type: RunIfFileMissing},
				}}
			},
			wantSub: "runIf.path is required",
		},
		{
			name: "fileMissing with relative path",
			mutate: func(s *Service) {
				*s = *withVolume(t)
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					RunIf: RunIf{Type: RunIfFileMissing, Path: "data/file"},
				}}
			},
			wantSub: "must be absolute",
		},
		{
			name: "freshVolume with no parent volume to anchor",
			mutate: func(s *Service) {
				// no volumes at all on the parent
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					RunIf: RunIf{Type: RunIfFreshVolume},
				}}
			},
			wantSub: "requires the step to mount at least one parent volume",
		},
		{
			name: "freshVolume with explicit empty volumes filter",
			mutate: func(s *Service) {
				*s = *withVolume(t)
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					Volumes: []string{}, // explicit empty: mount none
					RunIf:   RunIf{Type: RunIfFreshVolume},
				}}
			},
			wantSub: "requires the step to mount at least one parent volume",
		},
		{
			name: "freshVolume rejects path/volume fields",
			mutate: func(s *Service) {
				*s = *withVolume(t)
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					RunIf: RunIf{Type: RunIfFreshVolume, Path: "/data/x"},
				}}
			},
			wantSub: "only valid with runIf.type=fileMissing",
		},
		{
			name: "fileMissing volume not in filter",
			mutate: func(s *Service) {
				base := withVolume(t)
				base.Volumes = append(base.Volumes, VolumeMount{
					Name:      "logs",
					MountPath: "/logs",
					ClaimTemplate: &VolumeClaimTemplate{
						StorageClassName: "local-host",
						Size:             "1Gi",
						AccessMode:       AccessModeRWO,
					},
				})
				*s = *base
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					Volumes: []string{"data"},
					RunIf:   RunIf{Type: RunIfFileMissing, Path: "/logs/x", Volume: "logs"},
				}}
			},
			wantSub: "is not in the step's volumes filter",
		},
		{
			name: "unknown runIf type",
			mutate: func(s *Service) {
				*s = *withVolume(t)
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					RunIf: RunIf{Type: "whenever"},
				}}
			},
			wantSub: "unknown runIf.type",
		},
		{
			name: "unknown restart policy",
			mutate: func(s *Service) {
				*s = *withVolume(t)
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					RunIf:         RunIf{Type: RunIfAlways},
					RestartPolicy: "RestartUnlessStopped",
				}}
			},
			wantSub: "unknown restartPolicy",
		},
		{
			name: "negative timeout",
			mutate: func(s *Service) {
				*s = *withVolume(t)
				s.InitSteps = []InitStep{{
					Name: "x", Image: "i", Command: "c",
					RunIf:   RunIf{Type: RunIfAlways},
					Timeout: -1,
				}}
			},
			wantSub: "timeout cannot be negative",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := makeBaseService()
			tc.mutate(s)
			err := s.Validate()
			require.Error(t, err, "expected validation error")
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

func TestServiceValidate_InitSteps_ProcessRuntime(t *testing.T) {
	t.Run("process runtime with image is rejected", func(t *testing.T) {
		s := &Service{
			ID:        "svc-1",
			Name:      "p",
			Namespace: "default",
			Runtime:   RuntimeTypeProcess,
			Process:   &ProcessSpec{Command: "/bin/true"},
			Scale:     1,
			InitSteps: []InitStep{{
				Name:    "x",
				Image:   "should-not-have-image",
				Command: "/bin/echo",
				RunIf:   RunIf{Type: RunIfAlways},
			}},
		}
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "image must be empty")
	})

	t.Run("process runtime without image is accepted", func(t *testing.T) {
		s := &Service{
			ID:        "svc-1",
			Name:      "p",
			Namespace: "default",
			Runtime:   RuntimeTypeProcess,
			Process:   &ProcessSpec{Command: "/bin/true"},
			Scale:     1,
			InitSteps: []InitStep{{
				Name:    "x",
				Command: "/bin/echo",
				RunIf:   RunIf{Type: RunIfAlways},
			}},
		}
		require.NoError(t, s.Validate())
	})
}
