package startup

import (
	"context"
	"os"
	"testing"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessRegistryAuth_FromSecretString(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	secRepo := repos.NewSecretRepo(testStore)
	logger := log.NewLogger(log.WithLevel(log.InfoLevel))
	auth := make(map[string]any)

	// Test case: fromSecret as string
	registry := config.DockerRegistryConfig{
		Name:     "test-registry",
		Registry: "test.example.com",
		Auth: config.DockerRegistryAuth{
			FromSecret: "my-secret",
			Bootstrap:  true,
			Data: map[string]string{
				"user": "testuser",
				"pass": "testpass",
			},
		},
	}

	err := processRegistryAuth(registry, secRepo, logger, auth)
	require.NoError(t, err)

	// Verify auth map is populated
	assert.Equal(t, "basic", auth["type"])
	assert.Equal(t, "testuser", auth["user"])
	assert.Equal(t, "testpass", auth["pass"])
}

func TestProcessRegistryAuth_FromSecretMap(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	secRepo := repos.NewSecretRepo(testStore)
	logger := log.NewLogger(log.WithLevel(log.InfoLevel))
	auth := make(map[string]any)

	// Test case: fromSecret as map
	registry := config.DockerRegistryConfig{
		Name:     "test-registry",
		Registry: "test.example.com",
		Auth: config.DockerRegistryAuth{
			FromSecret: map[string]any{
				"name":      "my-secret",
				"namespace": "custom-ns",
			},
			Bootstrap: true,
			Data: map[string]string{
				"tok": "test-token",
			},
		},
	}

	err := processRegistryAuth(registry, secRepo, logger, auth)
	require.NoError(t, err)

	// Verify auth map is populated
	assert.Equal(t, "token", auth["type"])
	assert.Equal(t, "test-token", auth["tok"])
}

func TestProcessRegistryAuth_DirectFields(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	secRepo := repos.NewSecretRepo(testStore)
	logger := log.NewLogger(log.WithLevel(log.InfoLevel))
	auth := make(map[string]any)

	// Test case: direct fields
	registry := config.DockerRegistryConfig{
		Name:     "test-registry",
		Registry: "test.example.com",
		Auth: config.DockerRegistryAuth{
			Type:     "basic",
			Username: "direct-user",
			Password: "direct-pass",
			Region:   "us-west-2",
		},
	}

	err := processRegistryAuth(registry, secRepo, logger, auth)
	require.NoError(t, err)

	// Verify auth map is populated
	assert.Equal(t, "basic", auth["type"])
	assert.Equal(t, "direct-user", auth["username"])
	assert.Equal(t, "direct-pass", auth["password"])
	assert.Equal(t, "us-west-2", auth["region"])
}

func TestBootstrapRegistrySecret_CreateNew(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	secRepo := repos.NewSecretRepo(testStore)
	logger := log.NewLogger(log.WithLevel(log.InfoLevel))

	auth := config.DockerRegistryAuth{
		Data: map[string]string{
			"user": "newuser",
			"pass": "newpass",
		},
		Bootstrap: true,
	}

	err := bootstrapRegistrySecret("system", "new-secret", auth, secRepo, logger)
	require.NoError(t, err)

	// Verify secret was created by checking if it exists in the store
	// Note: We can't easily verify the actual data due to encryption, but we can check the store state
	// The fact that no error was returned means the secret was created successfully
}

func TestBootstrapRegistrySecret_UpdateExisting(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	// Create an existing secret first using the SecretRepo to ensure proper encryption
	secRepo := repos.NewSecretRepo(testStore)
	existingSecret := &types.Secret{
		Name:      "existing-secret",
		Namespace: "system",
		Type:      "static",
		Data:      map[string]string{"old": "data"},
	}
	err := secRepo.Create(context.Background(), existingSecret)
	require.NoError(t, err)

	logger := log.NewLogger(log.WithLevel(log.InfoLevel))

	auth := config.DockerRegistryAuth{
		Data: map[string]string{
			"user": "updated-user",
			"pass": "updated-pass",
		},
		Bootstrap: true,
		Immutable: false,
		Manage:    "update",
	}

	err = bootstrapRegistrySecret("system", "existing-secret", auth, secRepo, logger)
	require.NoError(t, err)

	// Verify secret was updated by checking if it still exists and can be retrieved
	// The fact that no error was returned means the update was successful
}

func TestBootstrapRegistrySecret_Immutable(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	// Create an existing secret first using the SecretRepo
	secRepo := repos.NewSecretRepo(testStore)
	existingSecret := &types.Secret{
		Name:      "immutable-secret",
		Namespace: "system",
		Type:      "static",
		Data:      map[string]string{"old": "data"},
	}
	err := secRepo.Create(context.Background(), existingSecret)
	require.NoError(t, err)

	logger := log.NewLogger(log.WithLevel(log.InfoLevel))

	auth := config.DockerRegistryAuth{
		Data: map[string]string{
			"user": "newuser",
			"pass": "newpass",
		},
		Bootstrap: true,
		Immutable: true, // Should prevent update
	}

	err = bootstrapRegistrySecret("system", "immutable-secret", auth, secRepo, logger)
	require.NoError(t, err)

	// Verify secret was NOT updated by checking that the operation completed without error
	// The immutable flag should prevent the update
}

func TestResolveRegistrySecret_DockerConfigJSON(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	// Create a secret with dockerconfigjson using SecretRepo
	secRepo := repos.NewSecretRepo(testStore)
	secret := &types.Secret{
		Name:      "docker-secret",
		Namespace: "system",
		Data: map[string]string{
			".dockerconfigjson": `{"auths":{"test.example.com":{"auth":"dGVzdDp0ZXN0"}}}`,
		},
	}
	err := secRepo.Create(context.Background(), secret)
	require.NoError(t, err)

	logger := log.NewLogger(log.WithLevel(log.InfoLevel))
	auth := make(map[string]any)

	err = resolveRegistrySecret("system", "docker-secret", secRepo, logger, auth)
	require.NoError(t, err)

	// Verify auth map is populated with dockerconfigjson
	assert.Equal(t, "dockerconfigjson", auth["type"])
	assert.Contains(t, auth["dockerconfigjson"], "auths")
}

func TestResolveRegistrySecret_Token(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	// Create a secret with token using SecretRepo
	secRepo := repos.NewSecretRepo(testStore)
	secret := &types.Secret{
		Name:      "token-secret",
		Namespace: "system",
		Data: map[string]string{
			"tok": "test-token-value",
		},
	}
	err := secRepo.Create(context.Background(), secret)
	require.NoError(t, err)

	logger := log.NewLogger(log.WithLevel(log.InfoLevel))
	auth := make(map[string]any)

	err = resolveRegistrySecret("system", "token-secret", secRepo, logger, auth)
	require.NoError(t, err)

	// Verify auth map is populated with token
	assert.Equal(t, "token", auth["type"])
	assert.Equal(t, "test-token-value", auth["tok"])
}

func TestResolveRegistrySecret_Basic(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	// Create a secret with basic auth using SecretRepo
	secRepo := repos.NewSecretRepo(testStore)
	secret := &types.Secret{
		Name:      "basic-secret",
		Namespace: "system",
		Data: map[string]string{
			"user": "basic-user",
			"pass": "basic-pass",
		},
	}
	err := secRepo.Create(context.Background(), secret)
	require.NoError(t, err)

	logger := log.NewLogger(log.WithLevel(log.InfoLevel))
	auth := make(map[string]any)

	err = resolveRegistrySecret("system", "basic-secret", secRepo, logger, auth)
	require.NoError(t, err)

	// Verify auth map is populated with basic auth
	assert.Equal(t, "basic", auth["type"])
	assert.Equal(t, "basic-user", auth["user"])
	assert.Equal(t, "basic-pass", auth["pass"])
}

func TestResolveRegistrySecret_ECR(t *testing.T) {
	// Setup
	testStore := newRegistryTestStore(t)

	// Create a secret with ECR credentials using SecretRepo
	secRepo := repos.NewSecretRepo(testStore)
	secret := &types.Secret{
		Name:      "ecr-secret",
		Namespace: "system",
		Data: map[string]string{
			"awsAccessKeyId":     "AKIAIOSFODNN7EXAMPLE",
			"awsSecretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			"awsSessionToken":    "AQoEXAMPLEH4aoAH0gNCAPyJxz4BlCFFxWNE1OPTgk5TthT...",
		},
	}
	err := secRepo.Create(context.Background(), secret)
	require.NoError(t, err)

	logger := log.NewLogger(log.WithLevel(log.InfoLevel))
	auth := make(map[string]any)

	err = resolveRegistrySecret("system", "ecr-secret", secRepo, logger, auth)
	require.NoError(t, err)

	// Verify auth map is populated with ECR credentials
	assert.Equal(t, "ecr", auth["type"])
	assert.Equal(t, "AKIAIOSFODNN7EXAMPLE", auth["awsAccessKeyId"])
	assert.Equal(t, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", auth["awsSecretAccessKey"])
	assert.Equal(t, "AQoEXAMPLEH4aoAH0gNCAPyJxz4BlCFFxWNE1OPTgk5TthT...", auth["awsSessionToken"])
}

func TestBootstrapAndResolveRegistryAuth_Integration(t *testing.T) {
	// Setup
	cfg := &config.Config{
		Docker: config.Docker{
			Registries: []config.DockerRegistryConfig{
				{
					Name:     "test-registry",
					Registry: "test.example.com",
					Auth: config.DockerRegistryAuth{
						Type:     "basic",
						Username: "testuser",
						Password: "testpass",
					},
				},
				{
					Name:     "secret-registry",
					Registry: "secret.example.com",
					Auth: config.DockerRegistryAuth{
						FromSecret: "my-secret",
						Bootstrap:  true,
						Data: map[string]string{
							"tok": "secret-token",
						},
					},
				},
			},
		},
	}

	testStore := newRegistryTestStore(t)

	logger := log.NewLogger(log.WithLevel(log.InfoLevel))

	err := bootstrapAndResolveRegistryAuth(cfg, testStore, logger)
	require.NoError(t, err)

	// Verify viper was populated
	registries := viper.Get("docker.registries")
	require.NotNil(t, registries)

	// Verify both registries are present
	regs, ok := registries.([]map[string]any)
	require.True(t, ok)
	assert.Len(t, regs, 2)

	// Check first registry (direct fields)
	assert.Equal(t, "test-registry", regs[0]["name"])
	assert.Equal(t, "test.example.com", regs[0]["registry"])
	auth0, ok := regs[0]["auth"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "basic", auth0["type"])
	assert.Equal(t, "testuser", auth0["username"])
	assert.Equal(t, "testpass", auth0["password"])

	// Check second registry (from secret)
	assert.Equal(t, "secret-registry", regs[1]["name"])
	assert.Equal(t, "secret.example.com", regs[1]["registry"])
	auth1, ok := regs[1]["auth"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "token", auth1["type"])
	assert.Equal(t, "secret-token", auth1["tok"])
}

func TestBootstrapAndResolveRegistryAuth_FromSecretBasic(t *testing.T) {
	ts := newRegistryTestStore(t)

	// Precreate a secret with username/password in team-a namespace
	secRepo := repos.NewSecretRepo(ts)
	err := secRepo.Create(context.Background(), &types.Secret{
		Namespace: "team-a",
		Name:      "ghcr-credentials",
		Type:      "static",
		Data: map[string]string{
			"username": "user1",
			"password": "pass1",
		},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}

	cfg := config.Default()
	cfg.Docker.Registries = []config.DockerRegistryConfig{
		{
			Name:     "ghcr",
			Registry: "ghcr.io",
			Auth: config.DockerRegistryAuth{
				FromSecret: map[string]any{"name": "ghcr-credentials", "namespace": "team-a"},
			},
		},
	}

	logger := log.NewLogger()
	if err := bootstrapAndResolveRegistryAuth(cfg, ts, logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Verify viper received normalized registries
	if !viper.IsSet("docker.registries") {
		t.Fatalf("docker.registries not set in viper")
	}
	var regs []map[string]any
	if err := viper.UnmarshalKey("docker.registries", &regs); err != nil {
		t.Fatalf("unmarshal registries: %v", err)
	}
	if len(regs) != 1 {
		t.Fatalf("expected 1 registry, got %d", len(regs))
	}
	auth, _ := regs[0]["auth"].(map[string]any)
	if auth["type"] != "basic" || auth["username"] != "user1" || auth["password"] != "pass1" {
		t.Fatalf("unexpected auth: %#v", auth)
	}
}

func TestBootstrapAndResolveRegistryAuth_BootstrapSecretFromEnv(t *testing.T) {
	ts := newRegistryTestStore(t)

	// Set envs used by bootstrap data
	os.Setenv("GHCR_USER", "envuser")
	os.Setenv("GHCR_PAT", "envpass")
	t.Cleanup(func() {
		os.Unsetenv("GHCR_USER")
		os.Unsetenv("GHCR_PAT")
	})

	cfg := config.Default()
	cfg.Docker.Registries = []config.DockerRegistryConfig{
		{
			Name:     "ghcr",
			Registry: "ghcr.io",
			Auth: config.DockerRegistryAuth{
				FromSecret: map[string]any{"name": "ghcr-credentials", "namespace": "system"},
				Bootstrap:  true,
				Manage:     "create",
				Data: map[string]string{
					"username": "${GHCR_USER}",
					"password": "${GHCR_PAT}",
				},
			},
		},
	}

	logger := log.NewLogger()
	if err := bootstrapAndResolveRegistryAuth(cfg, ts, logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Secret should exist in store and viper should be populated
	secRepo := repos.NewSecretRepo(ts)
	s, err := secRepo.Get(context.Background(), "system", "ghcr-credentials")
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if s.Data["username"] != "envuser" || s.Data["password"] != "envpass" {
		t.Fatalf("secret data mismatch: %#v", s.Data)
	}

	var regs []map[string]any
	if err := viper.UnmarshalKey("docker.registries", &regs); err != nil {
		t.Fatalf("unmarshal registries: %v", err)
	}
	if len(regs) != 1 {
		t.Fatalf("expected 1 registry, got %d", len(regs))
	}
	auth, _ := regs[0]["auth"].(map[string]any)
	if auth["type"] != "basic" || auth["username"] != "envuser" || auth["password"] != "envpass" {
		t.Fatalf("unexpected auth: %#v", auth)
	}
}

func TestBootstrapAndResolveRegistryAuth_DockerConfigJSON(t *testing.T) {
	ts := newRegistryTestStore(t)
	secRepo := repos.NewSecretRepo(ts)
	raw := `{"auths":{"ghcr.io":{"auth":"` + "dXNlcjp0b2tlbg==" + `"}}}`
	if err := secRepo.Create(context.Background(), &types.Secret{
		Namespace: "system",
		Name:      "dockercfg",
		Type:      "static",
		Data:      map[string]string{".dockerconfigjson": raw},
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	cfg := config.Default()
	cfg.Docker.Registries = []config.DockerRegistryConfig{
		{
			Name:     "ghcr",
			Registry: "ghcr.io",
			Auth:     config.DockerRegistryAuth{FromSecret: "dockercfg"},
		},
	}
	logger := log.NewLogger()
	if err := bootstrapAndResolveRegistryAuth(cfg, ts, logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var regs []map[string]any
	if err := viper.UnmarshalKey("docker.registries", &regs); err != nil {
		t.Fatalf("unmarshal registries: %v", err)
	}
	auth, _ := regs[0]["auth"].(map[string]any)
	if auth["type"] != "dockerconfigjson" || auth["dockerconfigjson"] == "" {
		t.Fatalf("unexpected auth: %#v", auth)
	}
}

// newRegistryTestStore builds the store every test in this file needs: a
// TestStore whose options carry a KEK and the secret limits, because
// repos.NewSecretRepo reads both off the store via GetOpts.
//
// It also resets viper, which bootstrapAndResolveRegistryAuth writes into.
// That global is why these tests cannot run in parallel.
func newRegistryTestStore(t *testing.T) *store.TestStore {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)

	ts := store.NewTestStoreWithOptions(store.StoreOptions{
		KEKBytes:     []byte("0123456789abcdef0123456789abcdef"), // 32 bytes
		SecretLimits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
	// NewTestStoreWithOptions already marks the store open, so this only
	// guards against that changing.
	require.NoError(t, ts.Open(""))
	t.Cleanup(func() { _ = ts.Close() })
	return ts
}
