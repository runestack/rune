package main

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
)

// helper to reset viper between tests
func resetViper() {
	viper.Reset()
}

func TestBootstrapAndResolveRegistryAuth_FromSecretBasic(t *testing.T) {
	resetViper()
	ts := store.NewTestStoreWithOptions(store.StoreOptions{
		SecretEncryptionEnabled: true,
		KEKBytes:                make([]byte, 32),
		SecretLimits:            store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})

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
	resetViper()
	ts := store.NewTestStoreWithOptions(store.StoreOptions{
		SecretEncryptionEnabled: true,
		KEKBytes:                make([]byte, 32),
		SecretLimits:            store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})

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
	resetViper()
	ts := store.NewTestStoreWithOptions(store.StoreOptions{
		SecretEncryptionEnabled: true,
		KEKBytes:                make([]byte, 32),
		SecretLimits:            store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
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
