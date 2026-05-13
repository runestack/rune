package types

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestServiceSpec_Dependencies_Unmarshal_StringAndStructured(t *testing.T) {
	yamlData := `
name: api
image: repo/api:latest
dependencies:
  - "db"                 # same ns
  - "cache.shared.rune"  # cross-ns FQDN
  - service: auth         # structured
    namespace: security
`
	var spec ServiceSpec
	if err := yaml.Unmarshal([]byte(yamlData), &spec); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate error: %v", err)
	}

	svc, err := spec.ToService()
	if err != nil {
		t.Fatalf("ToService error: %v", err)
	}

	if len(svc.Dependencies) != 3 {
		t.Fatalf("expected 3 deps, got %d", len(svc.Dependencies))
	}

	// Default namespace is "default" in ToService when not provided
	// dep0: "db" -> service=db, ns=default
	if svc.Dependencies[0].Service != "db" || svc.Dependencies[0].Namespace != "default" {
		t.Errorf("dep0 unexpected: %+v", svc.Dependencies[0])
	}
	// dep1: "cache.shared.rune" -> service=cache, ns=shared
	if svc.Dependencies[1].Service != "cache" || svc.Dependencies[1].Namespace != "shared" {
		t.Errorf("dep1 unexpected: %+v", svc.Dependencies[1])
	}
	// dep2: structured auth/security
	if svc.Dependencies[2].Service != "auth" || svc.Dependencies[2].Namespace != "security" {
		t.Errorf("dep2 unexpected: %+v", svc.Dependencies[2])
	}
}

func TestServiceSpec_Dependencies_Invalid(t *testing.T) {
	yamlData := `
name: api
image: repo/api
dependencies:
  - {}
`
	var spec ServiceSpec
	if err := yaml.Unmarshal([]byte(yamlData), &spec); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if err := spec.Validate(); err == nil {
		t.Fatalf("expected validate error, got nil")
	}
}

func TestServiceSpec_EnvFrom_Normalization(t *testing.T) {
	yamlData := `
name: api
image: repo/api
namespace: default
envFrom:
  - secret: app-secrets
    prefix: APP_
  - configmap: app-config
`
	var spec ServiceSpec
	if err := yaml.Unmarshal([]byte(yamlData), &spec); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate error: %v", err)
	}
	svc, err := spec.ToService()
	if err != nil {
		t.Fatalf("ToService error: %v", err)
	}
	if len(svc.EnvFrom) != 2 {
		t.Fatalf("expected 2 envFrom entries, got %d", len(svc.EnvFrom))
	}
	if svc.EnvFrom[0].SecretName != "app-secrets" || svc.EnvFrom[0].Namespace != "default" || svc.EnvFrom[0].Prefix != "APP_" {
		t.Errorf("unexpected first envFrom: %+v", svc.EnvFrom[0])
	}
	if svc.EnvFrom[1].ConfigmapName != "app-config" || svc.EnvFrom[1].Namespace != "default" {
		t.Errorf("unexpected second envFrom: %+v", svc.EnvFrom[1])
	}
}

func TestServiceSpec_EnvFrom_Validation(t *testing.T) {
	// both secret and configmap set -> error
	yamlData := `
name: api
image: repo/api
envFrom:
  - secret: a
    configmap: b
`
	var spec ServiceSpec
	if err := yaml.Unmarshal([]byte(yamlData), &spec); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if err := spec.Validate(); err == nil {
		t.Fatalf("expected validate error, got nil")
	}
}

func TestServiceSpec_EnvFrom_Shorthand_Unmarshal(t *testing.T) {
	yamlData := `
service:
  name: api
  image: repo/api
  envFrom: {{secret:env-secret}}
`
	cf, err := ParseCastFileFromBytes([]byte(yamlData), "")
	if err != nil {
		t.Fatalf("parse cast file error: %v", err)
	}
	services, err := cf.GetServices()
	if err != nil {
		t.Fatalf("get services error: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	svc := services[0]
	if len(svc.EnvFrom) != 1 {
		t.Fatalf("expected 1 envFrom entries, got %d", len(svc.EnvFrom))
	}
	if svc.EnvFrom[0].SecretName != "env-secret" {
		t.Errorf("expected envFrom secret=env-secret, got %+v", svc.EnvFrom[0])
	}
}

func TestServiceSpec_EnvFrom_Mixed_Shorthand_Unmarshal(t *testing.T) {
	yamlData := `
service:
  name: api
  image: repo/api
  envFrom:
    - {{secret:env-secret}}
    - configmap: env-config
      prefix: APP_
  env:
    APP_MODE: production
`
	cf, err := ParseCastFileFromBytes([]byte(yamlData), "")
	if err != nil {
		t.Fatalf("parse cast file error: %v", err)
	}
	services, err := cf.GetServices()
	if err != nil {
		t.Fatalf("get services error: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	svc := services[0]
	if len(svc.EnvFrom) != 2 {
		t.Fatalf("expected 2 envFrom entries, got %d", len(svc.EnvFrom))
	}
	if svc.EnvFrom[0].SecretName != "env-secret" {
		t.Errorf("expected envFrom secret=env-secret, got %+v", svc.EnvFrom[0])
	}
	if svc.EnvFrom[1].ConfigmapName != "env-config" {
		t.Errorf("expected envFrom configmap=env-config, got %+v", svc.EnvFrom[1])
	}
	if svc.EnvFrom[1].Prefix != "APP_" {
		t.Errorf("expected envFrom prefix=APP_, got %+v", svc.EnvFrom[1])
	}
	if svc.Env["APP_MODE"] != "production" {
		t.Errorf("expected env APP_MODE=production, got %+v", svc.Env)
	}
}

// Regression for RUNE-121: initSteps must be in the structural-validation
// allowlist so cast files using it don't get rejected with
// "unknown field 'initSteps' in service specification".
func TestServiceSpec_InitSteps_StructuralValidation(t *testing.T) {
	yamlData := `
service:
  name: tigerbeetle
  namespace: shared
  image: ghcr.io/tigerbeetle/tigerbeetle:0.16.30
  command: "/tigerbeetle start --addresses=0.0.0.0:3000 /data/0_0.tigerbeetle"
  initSteps:
    - name: format
      image: ghcr.io/tigerbeetle/tigerbeetle:0.16.30
      command: "/tigerbeetle format --cluster=0 --replica=0 --replica-count=1 /data/0_0.tigerbeetle"
      runIf:
        type: fileMissing
        path: /data/0_0.tigerbeetle
`
	cf, err := ParseCastFileFromBytes([]byte(yamlData), "")
	if err != nil {
		t.Fatalf("parse cast file error: %v", err)
	}
	if perrs := cf.GetParseErrors(); len(perrs) > 0 {
		t.Fatalf("parse errors: %v", perrs)
	}
	if _, err := cf.GetServices(); err != nil {
		t.Fatalf("get services error: %v", err)
	}
	if len(cf.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(cf.Services))
	}
	if err := cf.Services[0].Validate(); err != nil {
		t.Fatalf("Validate() rejected initSteps: %v", err)
	}

	svcs, err := cf.GetServices()
	if err != nil {
		t.Fatalf("GetServices: %v", err)
	}
	if len(svcs) != 1 || len(svcs[0].InitSteps) != 1 {
		t.Fatalf("expected 1 service with 1 init step, got %d services / %d steps",
			len(svcs), func() int {
				if len(svcs) == 0 {
					return 0
				}
				return len(svcs[0].InitSteps)
			}())
	}
	step := svcs[0].InitSteps[0]
	if step.Name != "format" || step.Image == "" || step.Command == "" {
		t.Fatalf("init step not plumbed through ToService: %+v", step)
	}
	if step.RunIf.Type != RunIfFileMissing || step.RunIf.Path != "/data/0_0.tigerbeetle" {
		t.Fatalf("init step runIf not preserved: %+v", step.RunIf)
	}
}
