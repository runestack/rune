package service

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/spf13/viper"
)

func TestRegistryAdmin_AddListGetUpdateRemove(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	st := store.NewTestStoreWithOptions(store.StoreOptions{})
	svc := NewAdminService(st, log.GetDefaultLogger())
	ctx := context.Background()

	// start from empty
	viper.Set("docker.registries", []map[string]any{})

	// add
	_, err := svc.AddRegistry(ctx, &generated.AddRegistryRequest{Registry: &generated.RegistryConfig{
		Name:     "ghcr-main",
		Registry: "ghcr.io",
		Auth:     &generated.RegistryAuthConfig{Username: "${GHCR_USER}", Password: "${GHCR_PAT}", Bootstrap: true, Manage: "update"},
	}})
	if err != nil {
		t.Fatalf("add error: %v", err)
	}

	// list
	lr, err := svc.ListRegistries(ctx, &generated.ListRegistriesRequest{})
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if len(lr.GetRegistries()) != 1 {
		t.Fatalf("expected 1 registry, got %d", len(lr.GetRegistries()))
	}

	// get
	gr, err := svc.GetRegistry(ctx, &generated.GetRegistryRequest{Name: "ghcr-main"})
	if err != nil {
		t.Fatalf("get error: %v", err)
	}
	if gr.Registry.GetRegistry() != "ghcr.io" {
		t.Fatalf("unexpected registry host: %s", gr.Registry.GetRegistry())
	}

	// update
	_, err = svc.UpdateRegistry(ctx, &generated.UpdateRegistryRequest{Registry: &generated.RegistryConfig{
		Name:     "ghcr-main",
		Registry: "ghcr.io",
		Auth:     &generated.RegistryAuthConfig{Token: "abc", Manage: "ignore"},
	}})
	if err != nil {
		t.Fatalf("update error: %v", err)
	}

	// remove
	_, err = svc.RemoveRegistry(ctx, &generated.RemoveRegistryRequest{Name: "ghcr-main"})
	if err != nil {
		t.Fatalf("remove error: %v", err)
	}
	lr2, _ := svc.ListRegistries(ctx, &generated.ListRegistriesRequest{})
	if len(lr2.GetRegistries()) != 0 {
		t.Fatalf("expected 0 after remove, got %d", len(lr2.GetRegistries()))
	}
}

func TestRegistryAdmin_BootstrapAuth_Normalize(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	st := store.NewTestStoreWithOptions(store.StoreOptions{})
	svc := NewAdminService(st, log.GetDefaultLogger())
	ctx := context.Background()
	viper.Set("docker.registries", []map[string]any{{
		"name":     "dockerhub",
		"registry": "index.docker.io",
		"auth": map[string]any{
			"username": "${DOCKER_USER}",
			"password": "${DOCKER_PASS}",
		},
	}})
	_, err := svc.BootstrapAuth(ctx, &generated.BootstrapAuthRequest{All: true})
	if err != nil {
		t.Fatalf("bootstrap error: %v", err)
	}
	lr, _ := svc.ListRegistries(ctx, &generated.ListRegistriesRequest{})
	if len(lr.GetRegistries()) != 1 {
		t.Fatalf("expected 1 registry")
	}
	// type should be inferred
	if lr.GetRegistries()[0].GetAuth().GetType() == "" {
		t.Fatalf("expected inferred type")
	}
}

func TestAdminBootstrap_LocalOnlyDefault(t *testing.T) {
	st := store.NewTestStore()
	t.Cleanup(func() { st.Close() })
	svc := NewAdminService(st, nil)

	// Should succeed when no users exist (peer check best-effort; not enforced in test)
	resp, err := svc.AdminBootstrap(context.Background(), nil)
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	if resp.TokenSecret == "" {
		t.Fatalf("expected token secret")
	}
}

func TestPolicyCRUD_MVP(t *testing.T) {
	st := store.NewTestStore()
	t.Cleanup(func() { st.Close() })
	svc := NewAdminService(st, nil)

	// Create
	_, err := svc.PolicyCreate(context.Background(), &generated.PolicyCreateRequest{Policy: &generated.Policy{Name: "dev"}})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	// Get
	if _, err := svc.PolicyGet(context.Background(), &generated.PolicyGetRequest{Name: "dev"}); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	// List
	if _, err := svc.PolicyList(context.Background(), &generated.PolicyListRequest{}); err != nil {
		t.Fatalf("list policy: %v", err)
	}
	// Delete
	if _, err := svc.PolicyDelete(context.Background(), &generated.PolicyDeleteRequest{Name: "dev"}); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
}
