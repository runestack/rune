package service

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestScaleService(t *testing.T) {
	// Create service service with logger and fake orchestrator
	logger := log.NewTestLogger()
	fakeOrchestrator := orchestrator.NewFakeOrchestrator()
	store := store.NewTestStore()
	svc := NewServiceService(store, fakeOrchestrator, nil, logger)

	// Create test service
	testService := &types.Service{
		ID:        "test-service-id",
		Name:      "test-service",
		Namespace: "default",
		Scale:     1,
		Metadata: &types.ServiceMetadata{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}

	// Store test service in orchestrator (ServiceService reads via orchestrator)
	ctx := context.Background()
	require.NoError(t, fakeOrchestrator.CreateService(ctx, testService))

	// Test cases
	testCases := []struct {
		name      string
		request   *generated.ScaleServiceRequest
		wantScale int
		wantErr   codes.Code
	}{
		{
			name: "scale_up",
			request: &generated.ScaleServiceRequest{
				Name:      testService.Name,
				Namespace: testService.Namespace,
				Scale:     3,
			},
			wantScale: 3,
			wantErr:   codes.OK,
		},
		{
			name: "scale_down",
			request: &generated.ScaleServiceRequest{
				Name:      testService.Name,
				Namespace: testService.Namespace,
				Scale:     0,
			},
			wantScale: 0,
			wantErr:   codes.OK,
		},
		{
			name: "invalid_scale",
			request: &generated.ScaleServiceRequest{
				Name:      testService.Name,
				Namespace: testService.Namespace,
				Scale:     -1,
			},
			wantScale: 0, // Previous value preserved
			wantErr:   codes.InvalidArgument,
		},
		{
			name: "service_not_found",
			request: &generated.ScaleServiceRequest{
				Name:      "nonexistent-service",
				Namespace: testService.Namespace,
				Scale:     3,
			},
			wantScale: 0, // Not applicable
			wantErr:   codes.NotFound,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Call ScaleService
			resp, err := svc.ScaleService(ctx, tc.request)

			// Check error
			if tc.wantErr != codes.OK {
				require.Error(t, err)
				st, ok := status.FromError(err)
				require.True(t, ok, "expected gRPC status error")
				assert.Equal(t, tc.wantErr, st.Code(), "unexpected error code")
				return
			}

			// Check success
			require.NoError(t, err)
			require.NotNil(t, resp)
			require.NotNil(t, resp.Service)
			assert.Equal(t, tc.wantScale, int(resp.Service.Scale), "unexpected scale value")

			// Verify service was updated in the orchestrator
			updatedService, err := fakeOrchestrator.GetService(ctx, tc.request.Namespace, tc.request.Name)
			require.NoError(t, err)
			assert.Equal(t, tc.wantScale, updatedService.Scale, "service scale not updated")
		})
	}
}

func TestCreateService_DependencyCycleValidation(t *testing.T) {
	ctx := context.Background()
	logger := log.NewTestLogger()
	fake := orchestrator.NewFakeOrchestrator()
	store := store.NewTestStore()
	svc := NewServiceService(store, fake, nil, logger)

	// Seed two services forming a chain a->b
	a := &types.Service{Name: "a", Namespace: "default"}
	b := &types.Service{Name: "b", Namespace: "default"}
	a.Dependencies = []types.DependencyRef{{Service: "b", Namespace: "default"}}
	require.NoError(t, fake.CreateService(ctx, a))
	require.NoError(t, fake.CreateService(ctx, b))

	// Attempt to create c that depends on a (ok)
	cProto := &generated.Service{Name: "c", Namespace: "default", Dependencies: []*generated.DependencyRef{{Service: "a", Namespace: "default"}}}
	_, err := svc.CreateService(ctx, &generated.CreateServiceRequest{Service: cProto})
	require.NoError(t, err)

	// Now attempt to update b to depend on c, which would create a cycle a->b->c->a
	bProto := &generated.Service{Name: "b", Namespace: "default", Dependencies: []*generated.DependencyRef{{Service: "c", Namespace: "default"}}}
	_, err = svc.UpdateService(ctx, &generated.UpdateServiceRequest{Service: bProto})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestCreateService_DependencyCycleValidation_CreatePath(t *testing.T) {
	ctx := context.Background()
	logger := log.NewTestLogger()
	fake := orchestrator.NewFakeOrchestrator()
	store := store.NewTestStore()
	svc := NewServiceService(store, fake, nil, logger)

	// Seed a->b
	a := &types.Service{Name: "a", Namespace: "default"}
	b := &types.Service{Name: "b", Namespace: "default"}
	a.Dependencies = []types.DependencyRef{{Service: "b", Namespace: "default"}}
	require.NoError(t, fake.CreateService(ctx, a))
	require.NoError(t, fake.CreateService(ctx, b))

	// Try to create c that depends on a (ok)
	cProto := &generated.Service{Name: "c", Namespace: "default", Dependencies: []*generated.DependencyRef{{Service: "a", Namespace: "default"}}}
	_, err := svc.CreateService(ctx, &generated.CreateServiceRequest{Service: cProto})
	require.NoError(t, err)

	// Now try to create d that depends on c (ok), then update a to depend on d (cycle)
	dProto := &generated.Service{Name: "d", Namespace: "default", Dependencies: []*generated.DependencyRef{{Service: "c", Namespace: "default"}}}
	_, err = svc.CreateService(ctx, &generated.CreateServiceRequest{Service: dProto})
	require.NoError(t, err)

	aCycleProto := &generated.Service{Name: "a", Namespace: "default", Dependencies: []*generated.DependencyRef{{Service: "d", Namespace: "default"}}}
	_, err = svc.UpdateService(ctx, &generated.UpdateServiceRequest{Service: aCycleProto})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestDeleteService_BlocksWhenDependentsExist(t *testing.T) {
	ctx := context.Background()
	logger := log.NewTestLogger()
	fake := orchestrator.NewFakeOrchestrator()
	store := store.NewTestStore()
	svc := NewServiceService(store, fake, nil, logger)

	// Seed db and api depends on db
	db := &types.Service{Name: "db", Namespace: "default"}
	api := &types.Service{Name: "api", Namespace: "default", Dependencies: []types.DependencyRef{{Service: "db", Namespace: "default"}}}
	require.NoError(t, fake.CreateService(ctx, db))
	require.NoError(t, fake.CreateService(ctx, api))

	// Attempt to delete db without force
	_, err := svc.DeleteService(ctx, &generated.DeleteServiceRequest{Name: "db", Namespace: "default", Force: false})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.FailedPrecondition, st.Code())

	// Now with force (simulating --no-dependencies)
	_, err = svc.DeleteService(ctx, &generated.DeleteServiceRequest{Name: "db", Namespace: "default", Force: true})
	require.NoError(t, err)
}
