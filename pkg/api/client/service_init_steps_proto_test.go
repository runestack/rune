package client

import (
	"reflect"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// InitSteps and SecurityContext must round-trip cleanly through the
// proto layer. Pre-RUNE-121-followup the proto Service message had no
// init_steps field, so any flat-cast YAML that declared initSteps was
// silently dropped before reaching the controller. Lock both Bug A
// (init steps) and Bug B (security context) down with deep-equal.
func TestServiceProto_InitStepsRoundTrip(t *testing.T) {
	svc := &types.Service{
		Name:      "tigerbeetle",
		Namespace: "shared",
		Image:     "ghcr.io/tigerbeetle/tigerbeetle:0.16.30",
		Command:   "/tigerbeetle start --addresses=0.0.0.0:3000 /data/0_0.tigerbeetle",
		Scale:     1,
		Volumes: []types.VolumeMount{
			{
				Name:      "data",
				MountPath: "/data",
				Claim:     &types.VolumeClaim{Name: "tb-data"},
			},
		},
		InitSteps: []types.InitStep{
			{
				Name:    "format",
				Image:   "ghcr.io/tigerbeetle/tigerbeetle:0.16.30",
				Command: "/tigerbeetle format --cluster=0 --replica=0 --replica-count=1 /data/0_0.tigerbeetle",
				Args:    []string{"--verbose"},
				Env:     map[string]string{"TB_LOG": "debug"},
				RunIf: types.RunIf{
					Type: types.RunIfFileMissing,
					Path: "/data/0_0.tigerbeetle",
				},
				RestartPolicy: types.InitStepRestartNever,
				Timeout:       30 * time.Second,
				SecurityContext: &types.SecurityContext{
					SeccompProfile: &types.SeccompProfile{Type: types.SeccompProfileUnconfined},
					CapAdd:         []string{"SYS_NICE"},
				},
			},
			{
				Name:    "noop",
				Image:   "busybox",
				Command: "true",
				// Explicit empty filters: not the same as inheriting all.
				Volumes:         []string{},
				SecretMounts:    []string{},
				ConfigmapMounts: []string{},
				RunIf:           types.RunIf{Type: types.RunIfAlways},
			},
		},
		SecurityContext: &types.SecurityContext{
			Privileged: true,
			CapDrop:    []string{"ALL"},
		},
	}

	pb := ServiceToProto(svc)
	if pb == nil {
		t.Fatal("ServiceToProto returned nil")
	}
	if len(pb.InitSteps) != 2 {
		t.Fatalf("expected 2 init steps in proto, got %d", len(pb.InitSteps))
	}
	if pb.SecurityContext == nil || !pb.SecurityContext.Privileged {
		t.Fatalf("main SecurityContext.Privileged not preserved: %+v", pb.SecurityContext)
	}

	got, err := ProtoToService(pb)
	if err != nil {
		t.Fatalf("ProtoToService: %v", err)
	}

	if !reflect.DeepEqual(got.InitSteps, svc.InitSteps) {
		t.Fatalf("InitSteps round-trip mismatch.\nwant: %#v\n got: %#v", svc.InitSteps, got.InitSteps)
	}
	if !reflect.DeepEqual(got.SecurityContext, svc.SecurityContext) {
		t.Fatalf("SecurityContext round-trip mismatch.\nwant: %#v\n got: %#v", svc.SecurityContext, got.SecurityContext)
	}

	// Explicit nil vs empty-slice for filter fields must survive the
	// trip — that's why the proto carries a *_set flag.
	if got.InitSteps[1].Volumes == nil {
		t.Errorf("step[1].Volumes should be a non-nil empty slice (mount none), got nil (inherit all)")
	}
	if len(got.InitSteps[1].Volumes) != 0 {
		t.Errorf("step[1].Volumes should be empty, got %v", got.InitSteps[1].Volumes)
	}
}
