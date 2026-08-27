package types

import (
	"strings"
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

// Regression for the Propeller "Bug D" report: a securityContext
// block with a misspelled field name (`seccomp: unconfined` instead
// of `seccompProfile: { type: unconfined }`) used to unmarshal into
// an empty struct silently. The init step then ran with Docker's
// default seccomp profile and TigerBeetle's `format` failed with
// "io_uring is not available". Now `rune cast` rejects the YAML up
// front with a precise error and a "did you mean" hint.
func TestServiceSpec_SecurityContext_RejectsUnknownField(t *testing.T) {
	yamlData := `
service:
  name: tigerbeetle
  image: ghcr.io/tigerbeetle/tigerbeetle:latest
  command: /tigerbeetle
  securityContext:
    seccomp: unconfined
`
	cf, err := ParseCastFileFromBytes([]byte(yamlData), "")
	if err != nil {
		t.Fatalf("parse cast file: %v", err)
	}
	if len(cf.Specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(cf.Specs))
	}
	err = cf.Specs[0].Validate()
	if err == nil {
		t.Fatal("Validate() accepted unknown field 'seccomp'; expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown field 'seccomp'") {
		t.Errorf("error should name the offending field; got: %v", err)
	}
	if !strings.Contains(msg, "service.securityContext") {
		t.Errorf("error should locate the block; got: %v", err)
	}
	if !strings.Contains(msg, "did you mean 'seccompProfile.type'?") {
		t.Errorf("error should hint at the correct schema; got: %v", err)
	}
}

// The correct schema must still validate cleanly.
func TestServiceSpec_SecurityContext_AcceptsValidSchema(t *testing.T) {
	yamlData := `
service:
  name: tigerbeetle
  image: ghcr.io/tigerbeetle/tigerbeetle:latest
  command: /tigerbeetle
  securityContext:
    seccompProfile:
      type: unconfined
    capAdd: [SYS_NICE]
`
	cf, err := ParseCastFileFromBytes([]byte(yamlData), "")
	if err != nil {
		t.Fatalf("parse cast file: %v", err)
	}
	if err := cf.Specs[0].Validate(); err != nil {
		t.Fatalf("Validate() rejected valid securityContext: %v", err)
	}
}

// Wrong field inside seccompProfile is also caught (one nesting
// level deeper).
func TestServiceSpec_SeccompProfile_RejectsUnknownField(t *testing.T) {
	yamlData := `
service:
  name: api
  image: nginx
  securityContext:
    seccompProfile:
      kind: unconfined
`
	cf, err := ParseCastFileFromBytes([]byte(yamlData), "")
	if err != nil {
		t.Fatalf("parse cast file: %v", err)
	}
	err = cf.Specs[0].Validate()
	if err == nil {
		t.Fatal("expected error for unknown seccompProfile field")
	}
	if !strings.Contains(err.Error(), "unknown field 'kind' in service.securityContext.seccompProfile") {
		t.Errorf("error should pinpoint the nested block; got: %v", err)
	}
}

// A misspelled field inside an init step's securityContext is also
// caught — the inheritance fix in v0.0.1-dev.40 doesn't help if
// the per-step block is itself empty due to a typo.
func TestServiceSpec_InitStep_SecurityContext_RejectsUnknownField(t *testing.T) {
	yamlData := `
service:
  name: tb
  image: tb
  command: /tigerbeetle
  volumes:
    - name: data
      mountPath: /data
      claimTemplate: { storageClassName: local, size: "1Gi" }
  initSteps:
    - name: format
      image: tb
      command: /tigerbeetle
      args: [format, /data/0_0.tigerbeetle]
      securityContext:
        seccomp: unconfined
`
	cf, err := ParseCastFileFromBytes([]byte(yamlData), "")
	if err != nil {
		t.Fatalf("parse cast file: %v", err)
	}
	err = cf.Specs[0].Validate()
	if err == nil {
		t.Fatal("expected error for unknown field in init step's securityContext")
	}
	if !strings.Contains(err.Error(), "initSteps[0].securityContext") {
		t.Errorf("error should locate the init step's securityContext block; got: %v", err)
	}
}

func TestServiceSpec_ConfigmapMounts_Unmarshal(t *testing.T) {
	t.Parallel()

	cf, err := ParseCastFileFromBytes([]byte(`
service:
  name: api
  image: nginx:alpine
  scale: 1
  configmapMounts:
    - name: cfg
      mountPath: /etc/app
      configmapName: app-settings
`), "default")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cf.Services) != 1 {
		t.Fatalf("services: %d", len(cf.Services))
	}
	s := cf.Services[0]
	if err := s.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(s.ConfigmapMounts) != 1 || s.ConfigmapMounts[0].ConfigmapName != "app-settings" {
		t.Fatalf("configmapMounts: %+v", s.ConfigmapMounts)
	}
}

func TestServiceSpec_RejectsConfigMountsAlias(t *testing.T) {
	t.Parallel()

	cf, err := ParseCastFileFromBytes([]byte(`
service:
  name: api
  image: nginx:alpine
  scale: 1
  configMounts:
    - name: cfg
      mountPath: /etc/app
      configmapName: app-settings
`), "default")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = cf.Services[0].Validate()
	if err == nil {
		t.Fatal("expected validation error for unknown field configMounts")
	}
	if !strings.Contains(err.Error(), "unknown field 'configMounts'") {
		t.Fatalf("got: %v", err)
	}
}

// Validate runs its checks in a fixed order and returns the first failure, so
// which error a bad spec reports depends on that order. The slice in Validate
// makes reordering a one-line, silent edit, so this walks a spec that violates
// every check and repairs them one at a time: repairing check N must surface
// check N+1, which pins every boundary in the slice.
//
// It pins rule order *within* a helper only where the spec violates more than
// one of that helper's rules — drainSeconds and autoscale max below. The other
// helpers are pinned at their first rule only.
func TestServiceSpec_Validate_CheckOrder(t *testing.T) {
	steps := []struct {
		want   string
		repair func(*ServiceSpec)
	}{
		{"service name is required", func(s *ServiceSpec) { s.Name = "api" }},
		{"service image is required", func(s *ServiceSpec) { s.Image = "repo/api" }},
		{"service scale cannot be negative", func(s *ServiceSpec) { s.Scale = 1 }},
		{"drainSeconds must be between", func(s *ServiceSpec) { s.DrainSeconds = nil }},
		{"port name is required", func(s *ServiceSpec) { s.Ports[0].Name = "http" }},
		{"invalid health check", func(s *ServiceSpec) { s.Health.Liveness = &Probe{Type: "http", Path: "/healthz", Port: 8080} }},
		{"must specify exactly one of", func(s *ServiceSpec) { s.EnvFrom[0].Secret = "creds" }},
		{"must specify 'from' peers", func(s *ServiceSpec) {
			s.NetworkPolicy.Ingress[0].From = []NetworkPolicyPeer{{Service: "web"}}
		}},
		{"autoscale min cannot be negative", func(s *ServiceSpec) { s.Autoscale.Min = 1 }},
		{"autoscale max cannot be less than min", func(s *ServiceSpec) {
			s.Autoscale = &ServiceAutoscale{Enabled: true, Min: 1, Max: 3, Metric: "cpu", Target: "80"}
		}},
		{"must specify service, secret, or configmap", func(s *ServiceSpec) {
			s.Dependencies[0].Service = "db"
		}},
		{"cpu.request cannot be negative", func(s *ServiceSpec) { s.Resources.CPU.Request = "100m" }},
		{"expose port is required", func(s *ServiceSpec) { s.Expose.Port = "http" }},
		{"volume mount at index 0: name is required", func(s *ServiceSpec) {
			s.Volumes[0] = VolumeMount{Name: "data", MountPath: "/data", Claim: &VolumeClaim{Name: "data"}}
		}},
	}

	overDrain := MaxDrainSeconds + 1
	spec := ServiceSpec{
		Scale:         -1,
		DrainSeconds:  &overDrain,
		Ports:         []ServicePort{{Port: 8080}},
		Health:        &HealthCheck{Liveness: &Probe{Type: "http"}},
		EnvFrom:       []EnvFromSourceSpec{{}},
		NetworkPolicy: &ServiceNetworkPolicy{Ingress: []IngressRule{{}}},
		Autoscale:     &ServiceAutoscale{Enabled: true, Min: -1, Max: -5},
		Dependencies:  []ServiceDependency{{}},
		Resources:     &Resources{CPU: ResourceLimit{Request: "-1"}},
		Expose:        &ServiceExpose{},
		Volumes:       []VolumeMount{{MountPath: "/data"}},
	}

	for i, step := range steps {
		err := spec.Validate()
		if err == nil {
			t.Fatalf("step %d: expected %q, got no error — a check ran out of order or stopped firing", i, step.want)
		}
		if !strings.Contains(err.Error(), step.want) {
			t.Fatalf("step %d: got %q, want it to contain %q", i, err.Error(), step.want)
		}
		step.repair(&spec)
	}

	if err := spec.Validate(); err != nil {
		t.Fatalf("every check repaired, but Validate still failed: %v", err)
	}
}

// The unknown-field check runs before every rule above, and only it reports the
// offending line — so a cast with both a typo and a missing name must report the
// typo, or the user loses the line number.
func TestServiceSpec_Validate_StructureBeforeFields(t *testing.T) {
	cast := "service:\n  nam: api\n  image: repo/api\n"
	cf, err := ParseCastFileFromBytes([]byte(cast), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	specs := cf.GetServiceSpecs()
	if len(specs) != 1 {
		t.Fatalf("got %d services, want 1", len(specs))
	}
	err = specs[0].Validate()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("got %q, want the unknown-field error rather than a later check", err.Error())
	}
}

// ParseCPU and ParseMemory both accept a leading minus, so the negative check
// is the only thing rejecting "cpu.request: -1".
func TestServiceSpec_Validate_NegativeResources(t *testing.T) {
	cases := []struct {
		name      string
		resources Resources
		want      string
	}{
		{"cpu request", Resources{CPU: ResourceLimit{Request: "-1"}}, "cpu.request cannot be negative"},
		{"cpu limit", Resources{CPU: ResourceLimit{Limit: "-500m"}}, "cpu.limit cannot be negative"},
		{"memory request", Resources{Memory: ResourceLimit{Request: "-1Gi"}}, "memory.request cannot be negative"},
		{"memory limit", Resources{Memory: ResourceLimit{Limit: "-1Gi"}}, "memory.limit cannot be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.resources
			spec := ServiceSpec{Name: "api", Image: "repo/api", Resources: &res}
			err := spec.Validate()
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}
