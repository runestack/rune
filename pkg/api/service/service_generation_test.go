package service

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generationTestService carries a field from each half of the hash split.
func generationTestService() *types.Service {
	return &types.Service{
		ID:        "svc-id",
		Name:      "api",
		Namespace: "default",
		Image:     "app:v1",
		Runtime:   types.RuntimeTypeContainer,
		Scale:     2,
		Env:       map[string]string{"MODE": "prod"},
		Ports:     []types.ServicePort{{Name: "http", Port: 80, HostPort: 8080}},
		SecurityContext: &types.SecurityContext{
			Privileged: true,
			CapAdd:     []string{"SYS_ADMIN"},
		},
		InitSteps: []types.InitStep{{Name: "migrate", Command: "migrate"}},
		EnvFrom:   []types.EnvFromSource{{SecretName: "db"}},
		Health: &types.HealthCheck{Readiness: &types.Probe{
			Type: "tcp", Port: 80, IntervalSeconds: 10,
		}},
		Labels:   map[string]string{"team": "a"},
		Expose:   &types.ServiceExpose{Port: "http", Host: "api.example.com"},
		Metadata: &types.ServiceMetadata{Generation: 5, TemplateGeneration: 5, ObservedGeneration: 5},
	}
}

func updateGenerations(t *testing.T, mutate func(*types.Service)) (generation, templateGeneration int64) {
	t.Helper()
	ctx := context.Background()
	fake := orchestrator.NewFakeOrchestrator()
	svc := NewServiceService(store.NewTestStore(), fake, nil, log.NewTestLogger())
	require.NoError(t, fake.CreateService(ctx, generationTestService()))

	next := generationTestService()
	mutate(next)
	_, err := svc.UpdateService(ctx, &generated.UpdateServiceRequest{Service: client.ServiceToProto(next)})
	require.NoError(t, err)

	got, err := fake.GetService(ctx, "default", "api")
	require.NoError(t, err)
	return got.Metadata.Generation, got.Metadata.TemplateGeneration
}

// Nothing stores a hash: both counters move only when an update compares the
// stored spec against the incoming one. So a service written before the
// field classification changed is not rolled by the upgrade itself, and an
// unchanged spec sent after it moves neither counter. It goes through the
// proto, so a field this service sets that the proto dropped would show up
// here as a change.
func TestUpdateService_UnchangedSpecMovesNoCounter(t *testing.T) {
	gen, tmpl := updateGenerations(t, func(*types.Service) {})
	assert.Equal(t, int64(5), gen)
	assert.Equal(t, int64(5), tmpl)
}

func TestUpdateService_DesiredStateChangeDoesNotReplaceInstances(t *testing.T) {
	for name, mutate := range map[string]func(*types.Service){
		"label":       func(s *types.Service) { s.Labels["team"] = "b" },
		"expose host": func(s *types.Service) { s.Expose.Host = "api2.example.com" },
		"scale":       func(s *types.Service) { s.Scale = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			gen, tmpl := updateGenerations(t, mutate)
			assert.Equal(t, int64(6), gen, "the reconciler must see the change")
			assert.Equal(t, int64(5), tmpl, "no running instance needs replacing")
		})
	}
}

func TestUpdateService_TemplateChangeReplacesInstances(t *testing.T) {
	for name, mutate := range map[string]func(*types.Service){
		"drop privileged": func(s *types.Service) { s.SecurityContext.Privileged = false },
		"drop capability": func(s *types.Service) { s.SecurityContext.CapAdd = nil },
		"init step":       func(s *types.Service) { s.InitSteps[0].Command = "migrate --v2" },
		"envFrom":         func(s *types.Service) { s.EnvFrom[0].SecretName = "db2" },
		"hostPort":        func(s *types.Service) { s.Ports[0].HostPort = 8081 },
		"probe":           func(s *types.Service) { s.Health.Readiness.IntervalSeconds = 5 },
	} {
		t.Run(name, func(t *testing.T) {
			gen, tmpl := updateGenerations(t, mutate)
			assert.Equal(t, int64(6), gen)
			assert.Equal(t, int64(6), tmpl, "a running instance cannot pick this up")
		})
	}
}
