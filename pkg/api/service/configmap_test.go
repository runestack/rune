package service

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestConfigmapServicePatchVersionsAndRollback exercises the configmap parity
// surface: server-side merge (set/unset) under a per-configmap lock, version
// history, and rollback. Unlike secrets, configmap data is plaintext so the
// version list carries the data directly.
func TestConfigmapServicePatchVersionsAndRollback(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		ConfigLimits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
	svc := NewConfigmapService(st, log.GetDefaultLogger())

	// Create v1 with two keys.
	if _, err := svc.CreateConfigmap(ctx, &generated.CreateConfigmapRequest{
		Configmap: &generated.Configmap{
			Name: "app-config", Namespace: "prod",
			Data: map[string]string{"LOG_LEVEL": "info", "REGION": "us-east"},
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create v1: %v", err)
	}

	// Patch: set LOG_LEVEL + add a new key -> v2. REGION must be preserved.
	time.Sleep(5 * time.Millisecond)
	p2, err := svc.PatchConfigmap(ctx, &generated.PatchConfigmapRequest{
		Name: "app-config", Namespace: "prod",
		Set: map[string]string{"LOG_LEVEL": "debug", "TIMEOUT": "30s"},
	})
	if err != nil {
		t.Fatalf("patch set: %v", err)
	}
	if p2.Configmap.Version != 2 {
		t.Fatalf("patch set bumped to v%d, want v2", p2.Configmap.Version)
	}
	if p2.Configmap.Data["LOG_LEVEL"] != "debug" || p2.Configmap.Data["REGION"] != "us-east" || p2.Configmap.Data["TIMEOUT"] != "30s" {
		t.Fatalf("patch set did not merge correctly: %v", p2.Configmap.Data)
	}

	// Identical set is a no-op (no version bump).
	noop, err := svc.PatchConfigmap(ctx, &generated.PatchConfigmapRequest{
		Name: "app-config", Namespace: "prod", Set: map[string]string{"LOG_LEVEL": "debug"},
	})
	if err != nil {
		t.Fatalf("noop patch: %v", err)
	}
	if noop.Configmap.Version != 2 {
		t.Fatalf("no-op patch bumped to v%d, want v2", noop.Configmap.Version)
	}

	// Patch: unset REGION -> v3.
	time.Sleep(5 * time.Millisecond)
	p3, err := svc.PatchConfigmap(ctx, &generated.PatchConfigmapRequest{
		Name: "app-config", Namespace: "prod", Unset: []string{"REGION"},
	})
	if err != nil {
		t.Fatalf("patch unset: %v", err)
	}
	if p3.Configmap.Version != 3 {
		t.Fatalf("patch unset bumped to v%d, want v3", p3.Configmap.Version)
	}
	if _, ok := p3.Configmap.Data["REGION"]; ok {
		t.Fatalf("patch unset did not remove REGION: %v", p3.Configmap.Data)
	}

	// Versions: 3 entries, newest-first, plaintext data present.
	lv, err := svc.ListConfigmapVersions(ctx, &generated.ListConfigmapVersionsRequest{Name: "app-config", Namespace: "prod"})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(lv.Versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(lv.Versions))
	}
	if lv.Versions[0].Version != 3 || lv.Versions[2].Version != 1 {
		t.Fatalf("versions not newest-first: [%d..%d]", lv.Versions[0].Version, lv.Versions[2].Version)
	}
	if lv.Versions[2].Data["LOG_LEVEL"] != "info" {
		t.Fatalf("v1 data not retained: %v", lv.Versions[2].Data)
	}

	// Rollback to v1 -> v4 with v1's data.
	time.Sleep(5 * time.Millisecond)
	rb, err := svc.RollbackConfigmap(ctx, &generated.RollbackConfigmapRequest{Name: "app-config", Namespace: "prod", ToVersion: 1})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rb.Configmap.Version != 4 {
		t.Fatalf("rollback bumped to v%d, want v4", rb.Configmap.Version)
	}
	if rb.Configmap.Data["LOG_LEVEL"] != "info" || rb.Configmap.Data["REGION"] != "us-east" || len(rb.Configmap.Data) != 2 {
		t.Fatalf("rollback HEAD data = %v, want v1's two keys", rb.Configmap.Data)
	}

	// Rolling back to the current head must fail.
	if _, err := svc.RollbackConfigmap(ctx, &generated.RollbackConfigmapRequest{Name: "app-config", Namespace: "prod", ToVersion: 4}); err == nil {
		t.Fatalf("expected error rolling back to current HEAD")
	}

	// Empty patch is rejected.
	if _, err := svc.PatchConfigmap(ctx, &generated.PatchConfigmapRequest{Name: "app-config", Namespace: "prod"}); err == nil {
		t.Fatalf("expected error for empty patch")
	}
}
