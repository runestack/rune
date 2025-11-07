package service

import (
	"context"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
)

func TestConfigServiceCRUD(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		ConfigLimits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
	svc := NewConfigmapService(st, log.GetDefaultLogger())

	// Create
	_, err := svc.CreateConfigmap(ctx, &generated.CreateConfigmapRequest{
		Configmap: &generated.Configmap{
			Name:      "app-config",
			Namespace: "prod",
			Data:      map[string]string{"logLevel": "info"},
		},
		EnsureNamespace: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get
	getResp, err := svc.GetConfigmap(ctx, &generated.GetConfigmapRequest{Name: "app-config", Namespace: "prod"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if getResp.Configmap.Name != "app-config" {
		t.Fatalf("bad get name")
	}

	// List
	listResp, err := svc.ListConfigmaps(ctx, &generated.ListConfigmapsRequest{Namespace: "prod"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listResp.Configmaps) != 1 {
		t.Fatalf("expected 1 config, got %d", len(listResp.Configmaps))
	}

	// Update
	_, err = svc.UpdateConfigmap(ctx, &generated.UpdateConfigmapRequest{Configmap: &generated.Configmap{
		Name:      "app-config",
		Namespace: "prod",
		Data:      map[string]string{"logLevel": "debug"},
	}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Delete
	_, err = svc.DeleteConfigmap(ctx, &generated.DeleteConfigmapRequest{Name: "app-config", Namespace: "prod"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestConfigServiceNoEnsureNamespace(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		ConfigLimits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
	svc := NewConfigmapService(st, log.GetDefaultLogger())

	// Try to create configmap in non-existent namespace without EnsureNamespace
	_, err := svc.CreateConfigmap(ctx, &generated.CreateConfigmapRequest{
		Configmap: &generated.Configmap{
			Name:      "test-config",
			Namespace: "non-existent",
			Data:      map[string]string{"key": "value"},
		},
		EnsureNamespace: false,
	})
	if err == nil {
		t.Fatalf("expected error when creating configmap in non-existent namespace without EnsureNamespace")
	}

	// Verify the error message indicates namespace doesn't exist
	if !strings.Contains(err.Error(), "namespace") && !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected error about namespace not existing, got: %v", err)
	}
}
