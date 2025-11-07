package controllers

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/types"
)

// Minimal test to assert that services with dependencies are gated until deps are ready
func TestReconciler_DependencyReadinessGating(t *testing.T) {
	ctx := context.Background()
	testStore := setupStore(t)
	logger := log.NewTestLogger()
	rm := manager.NewTestRunnerManager(nil)

	instCtrl := NewFakeInstanceController()
	healthCtrl := NewHealthController(logger, testStore, rm, instCtrl)
	rec := newReconciler(testStore, instCtrl, healthCtrl, logger)

	// Create dependency service dep with no readiness probe; running instance implies ready
	dep := &types.Service{Name: "dep", Namespace: "default", Scale: 1}
	if err := testStore.Create(ctx, types.ResourceTypeService, dep.Namespace, dep.Name, dep); err != nil {
		t.Fatalf("store create dep: %v", err)
	}

	// Create target service api depending on dep
	api := &types.Service{Name: "api", Namespace: "default", Scale: 1, Dependencies: []types.DependencyRef{{Service: "dep", Namespace: "default"}}}
	if err := testStore.Create(ctx, types.ResourceTypeService, api.Namespace, api.Name, api); err != nil {
		t.Fatalf("store create api: %v", err)
	}

	// Reconcile: without running dep instance, api should not create instances
	if err := rec.reconcileService(ctx, api); err != nil {
		t.Fatalf("reconcile api: %v", err)
	}
	if len(instCtrl.CreateInstanceCalls) != 0 {
		t.Fatalf("expected no instance creation before deps ready")
	}

	// Add a running instance for dep; gating should pass on next reconcile
	depInst := &types.Instance{ID: "dep-1", Name: "dep-1", Namespace: "default", ServiceName: "dep", Status: types.InstanceStatusRunning}
	if err := testStore.Create(ctx, types.ResourceTypeInstance, depInst.Namespace, depInst.ID, depInst); err != nil {
		t.Fatalf("store create dep instance: %v", err)
	}

	if err := rec.reconcileService(ctx, api); err != nil {
		t.Fatalf("reconcile api after dep ready: %v", err)
	}
	if len(instCtrl.CreateInstanceCalls) == 0 {
		t.Fatalf("expected instance creation after deps ready")
	}
}

// Non-service dependencies (secrets, configmaps) should not gate instance creation
func TestReconciler_DependencyReadiness_NonService(t *testing.T) {
	ctx := context.Background()
	testStore := setupStore(t)
	logger := log.NewTestLogger()
	rm := manager.NewTestRunnerManager(nil)

	instCtrl := NewFakeInstanceController()
	healthCtrl := NewHealthController(logger, testStore, rm, instCtrl)
	rec := newReconciler(testStore, instCtrl, healthCtrl, logger)

	// Create required non-service dependencies
	cfg := &types.Configmap{Name: "cfg", Namespace: "default", Data: map[string]string{"k": "v"}}
	if err := testStore.Create(ctx, types.ResourceTypeConfigmap, cfg.Namespace, cfg.Name, cfg); err != nil {
		t.Fatalf("store create configmap: %v", err)
	}
	sec := &types.Secret{Name: "sec", Namespace: "default", Type: "static", Data: map[string]string{"p": "x"}}
	if err := testStore.Create(ctx, types.ResourceTypeSecret, sec.Namespace, sec.Name, sec); err != nil {
		t.Fatalf("store create secret: %v", err)
	}

	// Service depending only on non-service deps should proceed immediately
	api := &types.Service{
		Name:      "api2",
		Namespace: "default",
		Scale:     1,
		Dependencies: []types.DependencyRef{
			{Configmap: "cfg", Namespace: "default"},
			{Secret: "sec", Namespace: "default"},
		},
	}
	if err := testStore.Create(ctx, types.ResourceTypeService, api.Namespace, api.Name, api); err != nil {
		t.Fatalf("store create api2: %v", err)
	}

	if err := rec.reconcileService(ctx, api); err != nil {
		t.Fatalf("reconcile api2: %v", err)
	}
	if len(instCtrl.CreateInstanceCalls) == 0 {
		t.Fatalf("expected instance creation when only non-service deps are declared")
	}
}
