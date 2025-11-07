package types

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test parsing a cast YAML that includes a singular service plus plural
// secrets and configmaps. Ensures we get a populated CastFile back.
func TestParseCastFile_MixedForms(t *testing.T) {
	t.Parallel()

	yamlContent := `
 # single service
 service:
   name: api
   image: nginx:alpine
   ports:
     - name: http
       port: 80

 # multiple services
 services:
   - name: frontend
     image: nginx:alpine
     ports:
       - name: http-frontend
         port: 80
   - name: worker
     image: busybox
     scale: 2
     ports:
       - name: http-worker
         port: 80
   - name: cron
     image: busybox
     scale: 1

 # multiple secrets
 secrets:
   - name: db-password
     type: static
     data:
       password: s3cr3t
   - name: redis-password
     type: static
     data:
       password: s3cr3t-redis
   - name: mongo-password
     type: static
     data:
       password: s3cr3t-mongo

 # multiple configmaps
 configmaps:
   - name: app-config
     data:
       LOG_LEVEL: debug
   - name: web-config
     data:
       LOG_LEVEL: debug
   - name: api-config
     data:
       LOG_LEVEL: debug
 `

	dir := t.TempDir()
	file := filepath.Join(dir, "cast.yaml")
	if err := os.WriteFile(file, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write temp yaml: %v", err)
	}

	cf, err := ParseCastFile(file, "")
	if err != nil {
		t.Fatalf("ParseCastFile returned error: %v", err)
	}
	if cf == nil {
		t.Fatalf("ParseCastFile returned nil CastFile")
	}

	// Services should include one from singular 'service' and three from 'services' list
	if len(cf.Services) != 4 {
		t.Fatalf("expected 4 services, got %d", len(cf.Services))
	}
	// quick sanity on names
	names := map[string]bool{}
	for _, s := range cf.Services {
		names[s.Name] = true
	}
	for _, want := range []string{"api", "frontend", "worker", "cron"} {
		if !names[want] {
			t.Fatalf("expected service %q present, got names: %+v", want, names)
		}
	}

	// Secrets parsed from 'secrets' list
	if len(cf.Secrets) != 3 {
		t.Fatalf("expected 3 secret specs, got %d", len(cf.Secrets))
	}
	secretNames := map[string]bool{}
	for _, s := range cf.Secrets {
		secretNames[s.Name] = true
	}
	for _, want := range []string{"db-password", "redis-password", "mongo-password"} {
		if !secretNames[want] {
			t.Fatalf("expected secret %q present, got names: %+v", want, secretNames)
		}
	}

	// Configmaps parsed from 'configmaps' list
	if len(cf.Configmaps) != 3 {
		t.Fatalf("expected 3 configmap specs, got %d", len(cf.Configmaps))
	}
	cmNames := map[string]bool{}
	for _, c := range cf.Configmaps {
		cmNames[c.Name] = true
	}
	for _, want := range []string{"app-config", "web-config", "api-config"} {
		if !cmNames[want] {
			t.Fatalf("expected configmap %q present, got names: %+v", want, cmNames)
		}
	}

	// Validate conversion helpers
	secrets, err := cf.GetSecrets()
	if err != nil {
		t.Fatalf("GetSecrets returned error: %v", err)
	}
	if len(secrets) != 3 {
		t.Fatalf("expected 3 concrete secrets, got %d", len(secrets))
	}

	configs, err := cf.GetConfigmaps()
	if err != nil {
		t.Fatalf("GetConfigmaps returned error: %v", err)
	}
	if len(configs) != 3 {
		t.Fatalf("expected 3 concrete configmaps, got %d", len(configs))
	}
}

func TestParseCastFile(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "single service",
			yaml: `
service:
  name: test-service
  image: nginx:latest
  scale: 2
`,
			wantErr: false,
		},
		{
			name: "multiple services",
			yaml: `
services:
  - name: service-1
    image: nginx:latest
    scale: 1
  - name: service-2
    image: redis:latest
    scale: 3
`,
			wantErr: false,
		},
		{
			name: "no resources to parse",
			yaml: `
foo: bar
`,
			wantErr: true,
		},
		{
			name: "invalid yaml",
			yaml: `
service:
  name: test-service
  image: nginx:latest
  scale: 2
  - invalid
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Write yaml to a temp file because ParseCastFile expects a filename
			dir := t.TempDir()
			fp := filepath.Join(dir, "case.yaml")
			if err := os.WriteFile(fp, []byte(tt.yaml), 0o600); err != nil {
				t.Fatalf("failed to write temp yaml: %v", err)
			}
			cf, err := ParseCastFile(fp, "")
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCastFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			// Check that we have services
			services, err := cf.GetServices()
			if err != nil {
				t.Errorf("GetServices() error = %v", err)
				return
			}
			if len(services) == 0 {
				t.Error("No services found in parsed file")
			}
		})
	}
}

func TestParseCastFile_FullService(t *testing.T) {
	// Skip if short testing is enabled
	if testing.Short() {
		t.Skip("skipping file I/O test in short mode")
	}

	// Create a temporary file for testing
	content := `
service:
  name: test-service
  namespace: test-ns
  image: nginx:latest
  scale: 2
  env:
    FOO: bar
    BAR: baz
  ports:
    - name: http
      port: 80
    - name: https
      port: 443
  health:
    liveness:
      type: http
      path: /health
      port: 8080
  expose:
    port: http
    host: example.com
  autoscale:
    enabled: true
    min: 2
    max: 10
    metric: cpu
    target: 70%
  affinity:
    required:
      - region=us-east-1
    preferred:
      - zone=us-east-1a
  discovery:
    mode: load-balanced
`

	tmpDir, err := os.MkdirTemp("", "rune-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpFile := filepath.Join(tmpDir, "service.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}

	// Test parsing the file
	cf, err := ParseCastFile(tmpFile, "")
	if err != nil {
		t.Fatalf("ParseCastFile() error = %v", err)
	}

	// Verify the parsed content
	if cf.Services == nil {
		t.Fatal("Expected single service but got nil")
	}

	service := cf.Services[0]
	if service.Name != "test-service" {
		t.Errorf("Service name = %v, want %v", service.Name, "test-service")
	}

	if service.Namespace != "test-ns" {
		t.Errorf("Service namespace = %v, want %v", service.Namespace, "test-ns")
	}

	if service.Image != "nginx:latest" {
		t.Errorf("Service image = %v, want %v", service.Image, "nginx:latest")
	}

	if service.Scale != 2 {
		t.Errorf("Service scale = %v, want %v", service.Scale, 2)
	}

	if len(service.Env) != 2 {
		t.Errorf("Service env count = %v, want %v", len(service.Env), 2)
	}

	if service.Env["FOO"] != "bar" {
		t.Errorf("Service env FOO = %v, want %v", service.Env["FOO"], "bar")
	}

	// Check ports
	if len(service.Ports) != 2 {
		t.Errorf("Service ports count = %v, want %v", len(service.Ports), 2)
	} else {
		if service.Ports[0].Name != "http" || service.Ports[0].Port != 80 {
			t.Errorf("Port 'http' not parsed correctly: %+v", service.Ports[0])
		}
		if service.Ports[1].Name != "https" || service.Ports[1].Port != 443 {
			t.Errorf("Port 'https' not parsed correctly: %+v", service.Ports[1])
		}
	}

	if service.Health == nil || service.Health.Liveness == nil {
		t.Error("Expected health check but got nil")
	} else {
		probe := service.Health.Liveness
		if probe.Type != "http" || probe.Path != "/health" || probe.Port != 8080 {
			t.Errorf("Health check not parsed correctly: %+v", probe)
		}
	}

	// Check expose
	if service.Expose == nil {
		t.Error("Expected expose but got nil")
	} else {
		if service.Expose.Port != "http" || service.Expose.Host != "example.com" {
			t.Errorf("Expose not parsed correctly: %+v", service.Expose)
		}
	}

	// Check autoscale
	if service.Autoscale == nil {
		t.Error("Expected autoscale but got nil")
	} else {
		if !service.Autoscale.Enabled || service.Autoscale.Min != 2 || service.Autoscale.Max != 10 ||
			service.Autoscale.Metric != "cpu" || service.Autoscale.Target != "70%" {
			t.Errorf("Autoscale not parsed correctly: %+v", service.Autoscale)
		}
	}

	// Check affinity
	if service.Affinity == nil {
		t.Error("Expected affinity but got nil")
	} else {
		if len(service.Affinity.Required) != 1 || service.Affinity.Required[0] != "region=us-east-1" {
			t.Errorf("Affinity required not parsed correctly: %+v", service.Affinity.Required)
		}
		if len(service.Affinity.Preferred) != 1 || service.Affinity.Preferred[0] != "zone=us-east-1a" {
			t.Errorf("Affinity preferred not parsed correctly: %+v", service.Affinity.Preferred)
		}
	}

	// Check discovery
	if service.Discovery == nil {
		t.Error("Expected discovery but got nil")
	} else {
		if service.Discovery.Mode != "load-balanced" {
			t.Errorf("Discovery mode not parsed correctly: %+v", service.Discovery.Mode)
		}
	}

	// Test parsing a non-existent file
	_, err = ParseCastFile(filepath.Join(tmpDir, "non-existent.yaml"), "")
	if err == nil {
		t.Error("Expected error when parsing non-existent file, got nil")
	}
}

func TestParseCastFile_FullSecret(t *testing.T) {
	t.Parallel()
	yamlContent := `
secrets:
  - name: api-key
    type: static
    data:
      key: abc123
  - name: dyn-token
    type: dynamic
    engine:
      name: vault
    rotation:
      interval: 30d
`
	dir := t.TempDir()
	fp := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(fp, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write temp yaml: %v", err)
	}
	cf, err := ParseCastFile(fp, "")
	if err != nil {
		t.Fatalf("ParseCastFile returned error: %v", err)
	}
	if len(cf.Secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(cf.Secrets))
	}
	// Validate conversion
	secs, err := cf.GetSecrets()
	if err != nil {
		t.Fatalf("GetSecrets error: %v", err)
	}
	if len(secs) != 2 {
		t.Fatalf("expected 2 concrete secrets, got %d", len(secs))
	}
}

func TestParseCastFile_FullConfigmap(t *testing.T) {
	t.Parallel()
	yamlContent := `
configmaps:
  - name: app-config
    data:
      LOG_LEVEL: info
  - name: api-config
    data:
      TIMEOUT: "5s"
`
	dir := t.TempDir()
	fp := filepath.Join(dir, "configs.yaml")
	if err := os.WriteFile(fp, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write temp yaml: %v", err)
	}
	cf, err := ParseCastFile(fp, "")
	if err != nil {
		t.Fatalf("ParseCastFile returned error: %v", err)
	}
	if len(cf.Configmaps) != 2 {
		t.Fatalf("expected 2 configmaps, got %d", len(cf.Configmaps))
	}
	// Validate conversion
	cfgs, err := cf.GetConfigmaps()
	if err != nil {
		t.Fatalf("GetConfigmaps error: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("expected 2 concrete configmaps, got %d", len(cfgs))
	}
}

func TestCastFile_ServiceUnknownFields(t *testing.T) {
	t.Parallel()
	yamlContent := `
services:
  - name: s1
    image: nginx
    badField: oops
`
	dir := t.TempDir()
	fp := filepath.Join(dir, "svc_bad.yaml")
	if err := os.WriteFile(fp, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write temp yaml: %v", err)
	}
	cf, err := ParseCastFile(fp, "")
	if err != nil {
		t.Fatalf("ParseCastFile returned error: %v", err)
	}
	errs := cf.Lint()
	if len(errs) == 0 {
		t.Fatalf("expected structural validation errors, got none")
	}
}

func TestCastFile_DependencyCyclesWithinFile(t *testing.T) {
	t.Parallel()
	yamlContent := `
services:
  - name: a
    image: nginx
    dependencies:
      - b
  - name: b
    image: nginx
    dependencies:
      - c
  - name: c
    image: nginx
    dependencies:
      - a
`
	dir := t.TempDir()
	fp := filepath.Join(dir, "cycles.yaml")
	if err := os.WriteFile(fp, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write temp yaml: %v", err)
	}
	cf, err := ParseCastFile(fp, "")
	if err != nil {
		t.Fatalf("ParseCastFile returned error: %v", err)
	}
	errs := cf.Lint()
	foundCycle := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "dependency cycle detected") {
			foundCycle = true
			break
		}
	}
	if !foundCycle {
		t.Fatalf("expected cycle detection error, got: %+v", errs)
	}
}

func TestCastFile_ExternalDependenciesIgnoredForExistence(t *testing.T) {
	t.Parallel()
	yamlContent := `
services:
  - name: api
    image: nginx
    dependencies:
      - db.otherns
`
	dir := t.TempDir()
	fp := filepath.Join(dir, "external.yaml")
	if err := os.WriteFile(fp, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write temp yaml: %v", err)
	}
	cf, err := ParseCastFile(fp, "")
	if err != nil {
		t.Fatalf("ParseCastFile returned error: %v", err)
	}
	errs := cf.Lint()
	// Should not error just because external dep isn't present in the same file
	for _, e := range errs {
		if strings.Contains(e.Error(), "not found in this cast file") {
			t.Fatalf("did not expect existence error for external deps, got: %v", e)
		}
	}
}

func TestValidateDependencyCycles_Helper(t *testing.T) {
	// Build a small graph with a cycle: a->b->c->a and a tail d->e
	adj := map[string][]string{
		MakeDependencyNodeKey("ns", "a"): {MakeDependencyNodeKey("ns", "b")},
		MakeDependencyNodeKey("ns", "b"): {MakeDependencyNodeKey("ns", "c")},
		MakeDependencyNodeKey("ns", "c"): {MakeDependencyNodeKey("ns", "a")},
		MakeDependencyNodeKey("ns", "d"): {MakeDependencyNodeKey("ns", "e")},
		MakeDependencyNodeKey("ns", "e"): nil,
	}
	errs := DetectDependencyCycles(adj)
	if len(errs) == 0 {
		t.Fatalf("expected at least one cycle error, got none")
	}
}

func TestParseCastFile_TemplateSyntax(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		check   func(*testing.T, *CastFile)
	}{
		{
			name: "simple configmap template",
			yaml: `
service:
  name: template-test
  image: nginx:latest
  env:
    LOG_LEVEL: {{configmap:app-settings/log-level}}
`,
			wantErr: false,
			check: func(t *testing.T, cf *CastFile) {
				if len(cf.Services) != 1 {
					t.Fatalf("expected 1 service, got %d", len(cf.Services))
				}
				service := cf.Services[0]
				if service.Env["LOG_LEVEL"] != "{{configmap:app-settings/log-level}}" {
					t.Errorf("expected template to be restored, got: %s", service.Env["LOG_LEVEL"])
				}

				// Check template map
				templateMap := cf.GetTemplateMap()
				if len(templateMap) != 1 {
					t.Fatalf("expected 1 template, got %d", len(templateMap))
				}

				// Find the placeholder and verify it maps to the correct template
				var foundTemplate string
				for placeholder, templateRef := range templateMap {
					if templateRef == "configmap:app-settings/log-level" {
						foundTemplate = placeholder
						break
					}
				}
				if foundTemplate == "" {
					t.Fatal("template reference not found in template map")
				}
			},
		},
		{
			name: "multiple template types",
			yaml: `
service:
  name: multi-template-test
  image: nginx:latest
  env:
    LOG_LEVEL: {{configmap:app-settings/log-level}}
    DB_PASSWORD: {{secret:db-credentials/password}}
    API_KEY: {{secret:api-keys/main}}
`,
			wantErr: false,
			check: func(t *testing.T, cf *CastFile) {
				if len(cf.Services) != 1 {
					t.Fatalf("expected 1 service, got %d", len(cf.Services))
				}
				service := cf.Services[0]

				// Check all templates are restored
				expectedTemplates := map[string]string{
					"LOG_LEVEL":   "{{configmap:app-settings/log-level}}",
					"DB_PASSWORD": "{{secret:db-credentials/password}}",
					"API_KEY":     "{{secret:api-keys/main}}",
				}

				for key, expected := range expectedTemplates {
					if service.Env[key] != expected {
						t.Errorf("env %s: expected %s, got %s", key, expected, service.Env[key])
					}
				}

				// Check template map has all 3 templates
				templateMap := cf.GetTemplateMap()
				if len(templateMap) != 3 {
					t.Fatalf("expected 3 templates, got %d", len(templateMap))
				}
			},
		},
		{
			name: "namespaced templates",
			yaml: `
service:
  name: namespaced-test
  image: nginx:latest
  env:
    CONFIG: {{configmap:my-namespace.config-name/key}}
    SECRET: {{secret:other-namespace.secret-name/password}}
`,
			wantErr: false,
			check: func(t *testing.T, cf *CastFile) {
				if len(cf.Services) != 1 {
					t.Fatalf("expected 1 service, got %d", len(cf.Services))
				}
				service := cf.Services[0]

				// Check namespaced templates are restored
				if service.Env["CONFIG"] != "{{configmap:my-namespace.config-name/key}}" {
					t.Errorf("expected namespaced configmap template, got: %s", service.Env["CONFIG"])
				}
				if service.Env["SECRET"] != "{{secret:other-namespace.secret-name/password}}" {
					t.Errorf("expected namespaced secret template, got: %s", service.Env["SECRET"])
				}

				// Check template map
				templateMap := cf.GetTemplateMap()
				if len(templateMap) != 2 {
					t.Fatalf("expected 2 templates, got %d", len(templateMap))
				}
			},
		},
		{
			name: "complex template paths",
			yaml: `
service:
  name: complex-path-test
  image: nginx:latest
  env:
    FEATURE_FLAG: {{configmap:app.config.settings.feature-flags/enabled}}
    NESTED_SECRET: {{secret:app.secrets.database.credentials/root-password}}
`,
			wantErr: false,
			check: func(t *testing.T, cf *CastFile) {
				if len(cf.Services) != 1 {
					t.Fatalf("expected 1 service, got %d", len(cf.Services))
				}
				service := cf.Services[0]

				// Check complex paths are preserved
				if service.Env["FEATURE_FLAG"] != "{{configmap:app.config.settings.feature-flags/enabled}}" {
					t.Errorf("expected complex configmap path, got: %s", service.Env["FEATURE_FLAG"])
				}
				if service.Env["NESTED_SECRET"] != "{{secret:app.secrets.database.credentials/root-password}}" {
					t.Errorf("expected complex secret path, got: %s", service.Env["NESTED_SECRET"])
				}
			},
		},
		{
			name: "mixed content with templates",
			yaml: `
service:
  name: mixed-content-test
  image: nginx:latest
  env:
    SIMPLE: "plain-value"
    WITH_TEMPLATE: "prefix-{{configmap:app/name}}-suffix"
    MULTI_TEMPLATE: "{{configmap:app/version}}-{{secret:app/key}}"
    QUOTED_TEMPLATE: '{{configmap:app/setting}}'
`,
			wantErr: false,
			check: func(t *testing.T, cf *CastFile) {
				if len(cf.Services) != 1 {
					t.Fatalf("expected 1 service, got %d", len(cf.Services))
				}
				service := cf.Services[0]

				// Check mixed content is handled correctly
				if service.Env["SIMPLE"] != "plain-value" {
					t.Errorf("expected plain value, got: %s", service.Env["SIMPLE"])
				}
				if service.Env["WITH_TEMPLATE"] != "prefix-{{configmap:app/name}}-suffix" {
					t.Errorf("expected mixed content, got: %s", service.Env["WITH_TEMPLATE"])
				}
				if service.Env["MULTI_TEMPLATE"] != "{{configmap:app/version}}-{{secret:app/key}}" {
					t.Errorf("expected multi-template, got: %s", service.Env["MULTI_TEMPLATE"])
				}
				if service.Env["QUOTED_TEMPLATE"] != "{{configmap:app/setting}}" {
					t.Errorf("expected quoted template, got: %s", service.Env["QUOTED_TEMPLATE"])
				}

				// Check template map has all 4 templates
				templateMap := cf.GetTemplateMap()
				if len(templateMap) != 4 {
					t.Fatalf("expected 4 templates, got %d", len(templateMap))
				}
			},
		},
		{
			name: "templates in services sequence",
			yaml: `
services:
  - name: service-1
    image: nginx:latest
    env:
      CONFIG: {{configmap:app/config}}
  - name: service-2
    image: redis:latest
    env:
      SECRET: {{secret:app/secret}}
`,
			wantErr: false,
			check: func(t *testing.T, cf *CastFile) {
				if len(cf.Services) != 2 {
					t.Fatalf("expected 2 services, got %d", len(cf.Services))
				}

				// Check first service
				service1 := cf.Services[0]
				if service1.Env["CONFIG"] != "{{configmap:app/config}}" {
					t.Errorf("service1: expected template, got: %s", service1.Env["CONFIG"])
				}

				// Check second service
				service2 := cf.Services[1]
				if service2.Env["SECRET"] != "{{secret:app/secret}}" {
					t.Errorf("service2: expected template, got: %s", service2.Env["SECRET"])
				}

				// Check template map has both templates
				templateMap := cf.GetTemplateMap()
				if len(templateMap) != 2 {
					t.Fatalf("expected 2 templates, got %d", len(templateMap))
				}
			},
		},
		{
			name: "invalid template syntax",
			yaml: `
service:
  name: invalid-template-test
  image: nginx:latest
  env:
    BAD_TEMPLATE: {{configmap:app/config
    UNCLOSED: {{secret:app/secret
`,
			wantErr: true, // Should fail due to unclosed braces
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Write yaml to a temp file
			dir := t.TempDir()
			fp := filepath.Join(dir, "template_test.yaml")
			if err := os.WriteFile(fp, []byte(tt.yaml), 0o600); err != nil {
				t.Fatalf("failed to write temp yaml: %v", err)
			}

			cf, err := ParseCastFile(fp, "")
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCastFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return // Expected error, test passed
			}

			if tt.check != nil {
				tt.check(t, cf)
			}
		})
	}
}

func TestCastFile_TemplateRestoration(t *testing.T) {
	t.Parallel()

	yamlContent := `
service:
  name: restoration-test
  image: nginx:latest
  env:
    TEMPLATE_1: {{configmap:app/config1}}
    TEMPLATE_2: {{secret:app/secret1}}
    TEMPLATE_3: {{configmap:namespace.name/key}}
`

	dir := t.TempDir()
	fp := filepath.Join(dir, "restoration_test.yaml")
	if err := os.WriteFile(fp, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write temp yaml: %v", err)
	}

	cf, err := ParseCastFile(fp, "")
	if err != nil {
		t.Fatalf("ParseCastFile returned error: %v", err)
	}

	// Test template restoration functionality
	templateMap := cf.GetTemplateMap()
	if len(templateMap) != 3 {
		t.Fatalf("expected 3 templates, got %d", len(templateMap))
	}

	// Test RestoreTemplateReferences method
	originalContent := "This contains __TEMPLATE_PLACEHOLDER_1__ and __TEMPLATE_PLACEHOLDER_2__"
	restored := cf.RestoreTemplateReferences(originalContent)

	// Should contain the original template syntax
	if !strings.Contains(restored, "{{configmap:app/config1}}") {
		t.Error("restored content should contain first template")
	}
	if !strings.Contains(restored, "{{secret:app/secret1}}") {
		t.Error("restored content should contain second template")
	}

	// Should not contain placeholders
	if strings.Contains(restored, "__TEMPLATE_PLACEHOLDER_") {
		t.Error("restored content should not contain placeholders")
	}
}

func TestServiceSpec_TemplateMethods(t *testing.T) {
	t.Parallel()

	// Create a service spec with template placeholders
	spec := &ServiceSpec{
		Name:  "template-method-test",
		Image: "nginx:latest",
		Env: map[string]string{
			"PLAIN":     "value",
			"TEMPLATE1": "__TEMPLATE_PLACEHOLDER_1__",
			"TEMPLATE2": "__TEMPLATE_PLACEHOLDER_2__",
		},
	}

	templateMap := map[string]string{
		"__TEMPLATE_PLACEHOLDER_1__": "configmap:app/config1",
		"__TEMPLATE_PLACEHOLDER_2__": "secret:app/secret1",
	}

	// Test RestoreTemplateReferences
	spec.RestoreTemplateReferences(templateMap)

	// Check templates are restored
	if spec.Env["TEMPLATE1"] != "{{configmap:app/config1}}" {
		t.Errorf("expected restored template, got: %s", spec.Env["TEMPLATE1"])
	}
	if spec.Env["TEMPLATE2"] != "{{secret:app/secret1}}" {
		t.Errorf("expected restored template, got: %s", spec.Env["TEMPLATE2"])
	}

	// Check plain values are unchanged
	if spec.Env["PLAIN"] != "value" {
		t.Errorf("expected unchanged value, got: %s", spec.Env["PLAIN"])
	}

	// Test GetEnvWithTemplates (should return a copy with templates restored)
	originalSpec := &ServiceSpec{
		Name:  "original-test",
		Image: "nginx:latest",
		Env: map[string]string{
			"PLAIN":     "value",
			"TEMPLATE1": "__TEMPLATE_PLACEHOLDER_1__",
		},
	}

	restoredEnv := originalSpec.GetEnvWithTemplates(templateMap)

	// Check the copy has templates restored
	if restoredEnv["TEMPLATE1"] != "{{configmap:app/config1}}" {
		t.Errorf("expected restored template in copy, got: %s", restoredEnv["TEMPLATE1"])
	}

	// Check original is unchanged
	if originalSpec.Env["TEMPLATE1"] != "__TEMPLATE_PLACEHOLDER_1__" {
		t.Errorf("original should be unchanged, got: %s", originalSpec.Env["TEMPLATE1"])
	}
}
