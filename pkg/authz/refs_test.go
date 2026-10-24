package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/types"
)

func TestServiceReferenceRequirements(t *testing.T) {
	svc := &types.Service{
		Name:      "api",
		Namespace: "trial",
		Env: map[string]string{
			"OWN":    "{{secret:own/k}}",                     // own namespace: nothing
			"PROD":   "x-{{ secret:db.prod.rune/password }}", // fqdn form, with spaces
			"URI":    "{{rune://configmap/shared/cfg/url}}",
			"BROKEN": "{{not a ref}}",
		},
		EnvFrom: []types.EnvFromSource{
			{SecretName: "s", Namespace: "trial"},
			{ConfigmapName: "c", Namespace: "shared"},
		},
		InitSteps: []types.InitStep{{
			Name:    "migrate",
			EnvFrom: []types.EnvFromSource{{SecretName: "m", Namespace: "ops"}},
		}},
		Volumes: []types.VolumeMount{
			{Name: "own", Claim: &types.VolumeClaim{Name: "data"}},
			{Name: "theirs", Claim: &types.VolumeClaim{Name: "prod/data"}},
		},
		Expose: &types.ServiceExpose{
			TLS:        &types.ExposeServiceTLS{Secret: "prod/site-tls"},
			ClientCert: &types.ExposeClientCert{CASecret: "ca"},
		},
	}
	got := ServiceReferenceRequirements(svc)
	want := []NamespacedRequirement{
		{Requirement{"secrets", "reveal"}, "ops"},
		{Requirement{"secrets", "reveal"}, "prod"},
		{Requirement{"volumes", "get"}, "prod"},
		{Requirement{"configmaps", "get"}, "shared"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %v, want %v", i, got[i], want[i])
		}
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
