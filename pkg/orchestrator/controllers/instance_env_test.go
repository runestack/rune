package controllers

import (
	"testing"

	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInterpolateEnv_NonInterpolatedValue tests that regular environment variables are not modified
func TestInterpolateEnv_NonInterpolatedValue(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test interpolation
	instanceCtrl := controller

	// Test a regular environment variable value (no interpolation)
	val, err := instanceCtrl.interpolateEnv(ctx, "regular-value", "default")
	assert.NoError(t, err)
	assert.Equal(t, "regular-value", val)
}

// TestInterpolateEnv_TemplateSyntax tests template variable interpolation using table-driven tests
func TestInterpolateEnv_TemplateSyntax(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)

	// Get the concrete instanceController to test interpolation
	instanceCtrl := controller

	// Create test secrets and configmaps
	secret := &types.Secret{
		ID:        "test-secret",
		Name:      "test-secret",
		Namespace: "default",
		Data: map[string]string{
			"username": "admin",
			"password": "secret123",
		},
	}
	// Use SecretRepo to create, ensuring secrets are stored in encrypted StoredSecret form
	secretRepo := repos.NewSecretRepo(testStore)
	err := secretRepo.CreateRef(ctx, types.FormatRef(types.ResourceTypeSecret, "default", "test-secret"), secret)
	require.NoError(t, err)

	configmap := &types.Configmap{
		ID:        "test-config",
		Name:      "test-config",
		Namespace: "default",
		Data: map[string]string{
			"log-level": "debug",
			"app-name":  "test-app",
		},
	}
	err = testStore.Create(ctx, types.ResourceTypeConfigmap, "default", "test-config", configmap)
	require.NoError(t, err)

	tests := []struct {
		name        string
		input       string
		expected    string
		expectError bool
		errorMsg    string
	}{
		{
			name:     "Simple template variable",
			input:    "{{secret:test-secret/username}}",
			expected: "admin",
		},
		{
			name:     "Template variable with embedded text",
			input:    "{{configmap:test-config/app-name}}-prod",
			expected: "test-app-prod",
		},
		{
			name:     "Multiple template variables",
			input:    "{{secret:test-secret/username}}:{{secret:test-secret/password}}",
			expected: "admin:secret123",
		},
		{
			name:     "Template variable with default namespace",
			input:    "{{configmap:test-config/log-level}}",
			expected: "debug",
		},
		{
			name:     "Template variable with explicit namespace",
			input:    "{{configmap:test-config.default.rune/log-level}}",
			expected: "debug",
		},
		{
			name:     "Template variable with minimal shorthand",
			input:    "{{secret:test-secret/password}}",
			expected: "secret123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := instanceCtrl.interpolateEnv(ctx, tt.input, "default")
			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, val)
			}
		})
	}
}

// TestInterpolateEnv_Errors tests error cases for template interpolation using table-driven tests
func TestInterpolateEnv_Errors(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test interpolation
	instanceCtrl := controller

	tests := []struct {
		name        string
		input       string
		expected    string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "Missing key in template variable",
			input:       "{{secret:test-secret}}",
			expected:    "",
			expectError: true,
			errorMsg:    "must include a key for interpolation",
		},
		{
			name:        "Invalid template variable format",
			input:       "{{invalid:format}}",
			expected:    "",
			expectError: true,
			errorMsg:    "must include a key for interpolation",
		},
		{
			name:        "Unsupported resource type",
			input:       "{{service:test-service/name}}",
			expected:    "",
			expectError: true,
			errorMsg:    "unsupported resource type",
		},
		{
			name:     "Malformed template syntax",
			input:    "{{unclosed",
			expected: "{{unclosed", // Should return as-is since it's not valid template syntax
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := instanceCtrl.interpolateEnv(ctx, tt.input, "default")
			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
				assert.Equal(t, tt.expected, val)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, val)
			}
		})
	}
}

// TestPrepareEnvVars_WithoutInterpolation tests that environment variables without interpolation work correctly
func TestPrepareEnvVars_WithoutInterpolation(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test prepareEnvVars
	instanceCtrl := controller

	// Create a test service with regular environment variables (no interpolation needed)
	service := &types.Service{
		ID:        "test-service",
		Name:      "test-service",
		Namespace: "default",
		Env: map[string]string{
			"REGULAR_VAR": "regular-value",
			"ANOTHER_VAR": "another-value",
		},
		Ports: []types.ServicePort{
			{
				Name: "http",
				Port: 8080,
			},
		},
	}

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: "test-service",
	}

	// This should work since no interpolation is needed
	envVars, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	assert.NoError(t, err)
	assert.NotNil(t, envVars)

	// Check that regular env vars are preserved
	assert.Equal(t, "regular-value", envVars["REGULAR_VAR"])
	assert.Equal(t, "another-value", envVars["ANOTHER_VAR"])

	// Check built-in vars
	assert.Equal(t, "test-service", envVars["RUNE_SERVICE_NAME"])
	assert.Equal(t, "default", envVars["RUNE_SERVICE_NAMESPACE"])
	assert.Equal(t, "test-instance", envVars["RUNE_INSTANCE_ID"])

	// Check port vars
	assert.Equal(t, "8080", envVars["TEST_SERVICE_SERVICE_PORT"])
	assert.Equal(t, "8080", envVars["TEST_SERVICE_SERVICE_PORT_HTTP"])
}

// TestPrepareEnvVars_Basic tests the basic environment variable preparation functionality
func TestPrepareEnvVars_Basic(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test prepareEnvVars
	instanceCtrl := controller

	// Create a test service with some env vars and ports
	service := &types.Service{
		ID:        "test-service",
		Name:      "test-service",
		Namespace: "default",
		Env: map[string]string{
			"SERVICE_VAR1": "value1",
			"SERVICE_VAR2": "value2",
		},
		Ports: []types.ServicePort{
			{
				Name: "http",
				Port: 8080,
			},
			{
				Name: "metrics",
				Port: 9090,
			},
		},
	}

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-instance-1",
		Name:      "test-instance-1",
		Namespace: "default",
		ServiceID: "test-service",
	}

	// Prepare environment variables
	envVars, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	assert.NoError(t, err, "prepareEnvVars should not return an error")
	assert.NotNil(t, envVars, "Environment variables should not be nil")

	// Check service-defined vars
	assert.Equal(t, "value1", envVars["SERVICE_VAR1"])
	assert.Equal(t, "value2", envVars["SERVICE_VAR2"])

	// Check built-in vars
	assert.Equal(t, "test-service", envVars["RUNE_SERVICE_NAME"])
	assert.Equal(t, "default", envVars["RUNE_SERVICE_NAMESPACE"])
	assert.Equal(t, "test-instance-1", envVars["RUNE_INSTANCE_ID"])

	// Check normalized vars
	assert.Equal(t, "test-service.default.rune", envVars["TEST_SERVICE_SERVICE_HOST"])
	assert.Equal(t, "8080", envVars["TEST_SERVICE_SERVICE_PORT"])
	assert.Equal(t, "8080", envVars["TEST_SERVICE_SERVICE_PORT_HTTP"])
	assert.Equal(t, "9090", envVars["TEST_SERVICE_SERVICE_PORT_METRICS"])
}

// TestPrepareEnvVars_HyphenatedNames tests environment variable preparation with hyphenated names
func TestPrepareEnvVars_HyphenatedNames(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test prepareEnvVars
	instanceCtrl := controller

	// Create a test service with hyphenated names
	service := &types.Service{
		ID:        "test-hyphenated-service",
		Name:      "test-hyphenated-service",
		Namespace: "test-ns",
		Ports: []types.ServicePort{
			{
				Name: "api-port",
				Port: 8000,
			},
		},
	}

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-instance-2",
		Name:      "test-instance-2",
		Namespace: "test-ns",
		ServiceID: "test-hyphenated-service",
	}

	// Prepare environment variables
	envVars, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	assert.NoError(t, err, "prepareEnvVars should not return an error")
	assert.NotNil(t, envVars, "Environment variables should not be nil")

	// Check normalization of hyphenated names
	assert.Equal(t, "test-hyphenated-service.test-ns.rune", envVars["TEST_HYPHENATED_SERVICE_SERVICE_HOST"])
	assert.Equal(t, "8000", envVars["TEST_HYPHENATED_SERVICE_SERVICE_PORT"])
	assert.Equal(t, "8000", envVars["TEST_HYPHENATED_SERVICE_SERVICE_PORT_API_PORT"])
}

// TestPrepareEnvVars_WithTemplateInterpolation tests environment variable preparation with template interpolation
func TestPrepareEnvVars_WithTemplateInterpolation(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)

	// Get the concrete instanceController to test prepareEnvVars
	instanceCtrl := controller

	// Create test secrets and configmaps
	secret := &types.Secret{
		ID:        "db-credentials",
		Name:      "db-credentials",
		Namespace: "default",
		Data: map[string]string{
			"username": "dbuser",
			"password": "dbpass123",
		},
	}
	secretRepo := repos.NewSecretRepo(testStore)
	err := secretRepo.CreateRef(ctx, types.FormatRef(types.ResourceTypeSecret, "default", "db-credentials"), secret)
	require.NoError(t, err)

	configmap := &types.Configmap{
		ID:        "app-settings",
		Name:      "app-settings",
		Namespace: "default",
		Data: map[string]string{
			"log-level": "info",
			"app-name":  "my-app",
		},
	}
	err = testStore.Create(ctx, types.ResourceTypeConfigmap, "default", "app-settings", configmap)
	require.NoError(t, err)

	// Create a test service with template interpolation in environment variables
	service := &types.Service{
		ID:        "test-service-templates",
		Name:      "test-service-templates",
		Namespace: "default",
		Env: map[string]string{
			"DB_USERNAME": "{{secret:db-credentials/username}}",
			"DB_PASSWORD": "{{secret:db-credentials/password}}",
			"LOG_LEVEL":   "{{configmap:app-settings/log-level}}",
			"APP_NAME":    "{{configmap:app-settings/app-name}}-v1",
		},
	}

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-instance-templates",
		Name:      "test-instance-templates",
		Namespace: "default",
		ServiceID: "test-service-templates",
	}

	// Prepare environment variables
	envVars, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	assert.NoError(t, err, "prepareEnvVars should not return an error")
	assert.NotNil(t, envVars, "Environment variables should not be nil")

	// Check that template variables were interpolated correctly
	assert.Equal(t, "dbuser", envVars["DB_USERNAME"])
	assert.Equal(t, "dbpass123", envVars["DB_PASSWORD"])
	assert.Equal(t, "info", envVars["LOG_LEVEL"])
	assert.Equal(t, "my-app-v1", envVars["APP_NAME"])

	// Check built-in vars are still present
	assert.Equal(t, "test-service-templates", envVars["RUNE_SERVICE_NAME"])
	assert.Equal(t, "default", envVars["RUNE_SERVICE_NAMESPACE"])
	assert.Equal(t, "test-instance-templates", envVars["RUNE_INSTANCE_ID"])
}

// TestPrepareEnvVars_EnvFrom covers import, prefix and precedence rules
func TestPrepareEnvVars_EnvFrom(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	instanceCtrl := controller

	// Prepare secret and configmap
	secret := &types.Secret{ID: "env-secrets", Name: "env-secrets", Namespace: "default", Data: map[string]string{
		"USER":     "admin",
		"PASSWORD": "s3cr3t",
	}}
	secretRepo := repos.NewSecretRepo(testStore)
	err := secretRepo.CreateRef(ctx, types.FormatRef(types.ResourceTypeSecret, "default", "env-secrets"), secret)
	require.NoError(t, err)

	cfg := &types.Configmap{ID: "app-settings", Name: "app-settings", Namespace: "default", Data: map[string]string{
		"LOG_LEVEL": "debug",
	}}
	err = testStore.Create(ctx, types.ResourceTypeConfigmap, "default", "app-settings", cfg)
	require.NoError(t, err)

	service := &types.Service{
		ID:        "svc",
		Name:      "svc",
		Namespace: "default",
		EnvFrom: []types.EnvFromSource{
			{SecretName: "env-secrets", Namespace: "default", Prefix: "APP_"},
			{ConfigmapName: "app-settings", Namespace: "default"},
		},
		// Explicit env overrides imported
		Env: map[string]string{
			"APP_USER": "override",
		},
	}

	instance := &types.Instance{ID: "i1", Name: "i1", Namespace: "default", ServiceID: "svc"}

	env, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	require.NoError(t, err)

	// Imported with prefix
	assert.Equal(t, "s3cr3t", env["APP_PASSWORD"])
	// Configmap imported without prefix
	assert.Equal(t, "debug", env["LOG_LEVEL"])
	// Explicit env overrides imported key
	assert.Equal(t, "override", env["APP_USER"])

	// Invalid key detection: create a bad secret and expect failure
	badSecret := &types.Secret{ID: "bad", Name: "bad", Namespace: "default", Data: map[string]string{
		"bad-key": "x",
	}}
	err = secretRepo.CreateRef(ctx, types.FormatRef(types.ResourceTypeSecret, "default", "bad"), badSecret)
	require.NoError(t, err)

	badService := &types.Service{ID: "svc2", Name: "svc2", Namespace: "default", EnvFrom: []types.EnvFromSource{
		{SecretName: "bad", Namespace: "default"},
	}}
	_, err = instanceCtrl.prepareEnvVars(ctx, badService, instance)
	require.Error(t, err)
}
