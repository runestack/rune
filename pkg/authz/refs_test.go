package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/types"
)

func TestServiceReferenceRequirements(t *testing.T) {
	reveal := func(ns string) []NamespacedRequirement {
		return []NamespacedRequirement{{Requirement{"secrets", "reveal"}, ns}}
	}
	get := func(res, ns string) []NamespacedRequirement {
		return []NamespacedRequirement{{Requirement{res, "get"}, ns}}
	}
	reveal2 := func(res, ns string) []NamespacedRequirement {
		return []NamespacedRequirement{{Requirement{res, "reveal"}, ns}}
	}
	svc := func(mut func(*types.Service)) *types.Service {
		s := &types.Service{Name: "api", Namespace: "trial"}
		mut(s)
		return s
	}
	// One kind per case, so each collector path has to work on its own.
	cases := []struct {
		name string
		svc  *types.Service
		want []NamespacedRequirement
	}{
		{"env secret, fqdn with spaces", svc(func(s *types.Service) { s.Env = map[string]string{"A": "x-{{ secret:db.prod.rune/pw }}"} }), reveal("prod")},
		{"env configmap, rune uri", svc(func(s *types.Service) { s.Env = map[string]string{"A": "{{rune://configmap/shared/cfg/url}}"} }), get("configmaps", "shared")},
		{"env own namespace", svc(func(s *types.Service) { s.Env = map[string]string{"A": "{{secret:db/pw}}"} }), nil},
		{"env unparseable", svc(func(s *types.Service) { s.Env = map[string]string{"A": "{{not a ref}}"} }), nil},
		{"envFrom secret", svc(func(s *types.Service) { s.EnvFrom = []types.EnvFromSource{{SecretName: "s", Namespace: "prod"}} }), reveal("prod")},
		{"envFrom configmap", svc(func(s *types.Service) { s.EnvFrom = []types.EnvFromSource{{ConfigmapName: "c", Namespace: "shared"}} }), get("configmaps", "shared")},
		{"envFrom own namespace", svc(func(s *types.Service) { s.EnvFrom = []types.EnvFromSource{{SecretName: "s", Namespace: "trial"}} }), nil},
		{"init step envFrom", svc(func(s *types.Service) {
			s.InitSteps = []types.InitStep{{Name: "m", EnvFrom: []types.EnvFromSource{{SecretName: "m", Namespace: "ops"}}}}
		}), reveal("ops")},
		{"init step env", svc(func(s *types.Service) {
			s.InitSteps = []types.InitStep{{Name: "m", Env: map[string]string{"A": "{{secret:db.ops.rune/pw}}"}}}
		}), reveal("ops")},
		{"volume claim", svc(func(s *types.Service) {
			s.Volumes = []types.VolumeMount{{Name: "v", Claim: &types.VolumeClaim{Name: "prod/data"}}}
		}), reveal2("volumes", "prod")},
		{"volume claim own namespace", svc(func(s *types.Service) {
			s.Volumes = []types.VolumeMount{{Name: "v", Claim: &types.VolumeClaim{Name: "data"}}}
		}), nil},
		{"claim template restore", svc(func(s *types.Service) {
			s.Volumes = []types.VolumeMount{{Name: "v", ClaimTemplate: &types.VolumeClaimTemplate{Parameters: map[string]string{types.RestoreFromSnapshotParam: "prod/nightly"}}}}
		}), reveal2("snapshots", "prod")},
		{"claim template secret param", svc(func(s *types.Service) {
			s.Volumes = []types.VolumeMount{{Name: "v", ClaimTemplate: &types.VolumeClaimTemplate{Parameters: map[string]string{"token": "secret:do.prod.rune/token"}}}}
		}), reveal("prod")},
		{"claim template own-namespace secret param", svc(func(s *types.Service) {
			s.Volumes = []types.VolumeMount{{Name: "v", ClaimTemplate: &types.VolumeClaimTemplate{Parameters: map[string]string{"token": "secret:do/token"}}}}
		}), nil},
		{"expose tls secret", svc(func(s *types.Service) {
			s.Expose = &types.ServiceExpose{TLS: &types.ExposeServiceTLS{Secret: "prod/site-tls"}}
		}), reveal("prod")},
		{"expose client CA", svc(func(s *types.Service) {
			s.Expose = &types.ServiceExpose{ClientCert: &types.ExposeClientCert{CASecret: "secret:ca.prod.rune"}}
		}), reveal("prod")},
		{"expose own namespace", svc(func(s *types.Service) {
			s.Expose = &types.ServiceExpose{TLS: &types.ExposeServiceTLS{Secret: "site-tls"}}
		}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ServiceReferenceRequirements(tc.svc)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestVolumeReferenceRequirements(t *testing.T) {
	vol := &types.Volume{Name: "data", Namespace: "trial", Parameters: map[string]string{
		types.RestoreFromSnapshotParam: "prod/nightly",
		"token":                        "secret:do.ops.rune/token",
		"own":                          "secret:local/token",
		"plain":                        "fast",
	}}
	want := []NamespacedRequirement{
		{Requirement{"secrets", "reveal"}, "ops"},
		{Requirement{"snapshots", "reveal"}, "prod"},
	}
	got := VolumeReferenceRequirements(vol)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}

	ctx := authctx.WithSubject(context.Background(), "ci")
	if err := NewGate(&nsAuth{allow: map[string]bool{"secrets:reveal@ops": true}}).AdmitVolume(ctx, vol); !IsDenied(err) {
		t.Fatalf("want a snapshots:reveal denial in prod, got %v", err)
	}
	if err := NewGate(&nsAuth{allow: map[string]bool{"secrets:reveal@ops": true, "snapshots:reveal@prod": true}}).AdmitVolume(ctx, vol); err != nil {
		t.Fatal(err)
	}
}

func TestAdmitServiceChecksCrossNamespaceRefs(t *testing.T) {
	ctx := authctx.WithSubject(context.Background(), "ci")
	svc := &types.Service{Name: "api", Namespace: "trial", Env: map[string]string{"P": "{{secret:db.prod.rune/pw}}"}}

	auth := &nsAuth{allow: map[string]bool{"secrets:get@prod": true}}
	err := NewGate(auth).AdmitService(ctx, svc)
	var d *DeniedError
	if !errors.As(err, &d) || d.Resource != "secrets" || d.Verb != "reveal" || d.Namespace != "prod" {
		t.Fatalf("want secrets:reveal denied in prod, got %v", err)
	}

	auth = &nsAuth{allow: map[string]bool{"secrets:reveal@prod": true}}
	if err := NewGate(auth).AdmitService(ctx, svc); err != nil {
		t.Fatalf("holder of secrets:reveal in prod was denied: %v", err)
	}

	// Own-namespace references need no extra grant.
	own := &types.Service{Name: "api", Namespace: "trial", Env: map[string]string{"P": "{{secret:db/pw}}"}}
	auth = &nsAuth{allow: map[string]bool{}}
	if err := NewGate(auth).AdmitService(ctx, own); err != nil || len(auth.asked) != 0 {
		t.Fatalf("own-namespace ref: err=%v asked=%v", err, auth.asked)
	}
}
