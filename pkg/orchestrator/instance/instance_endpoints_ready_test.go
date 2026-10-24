package instance

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturePublisher struct{ eps []types.Endpoint }

func (p *capturePublisher) PublishService(_ context.Context, _ *types.Service, eps []types.Endpoint) error {
	p.eps = eps
	return nil
}

func (p *capturePublisher) PublishLocalInstances(context.Context, string, map[string]types.InstanceIdentity) error {
	return nil
}

// An instance withdrawn by its readiness probe stays in the published set
// but unhealthy; the dataplane routes only to healthy endpoints.
func TestRepublishService_UnreadyInstanceIsUnhealthy(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestStore()
	rm := manager.NewTestRunnerManager(nil)
	tr := runner.NewTestRunner()
	rm.SetDockerRunner(tr)
	rm.SetProcessRunner(tr)
	c := NewController(st, rm, log.NewLogger())
	pub := &capturePublisher{}
	c.SetEndpointPublisher(pub, "node-test")

	svc := &types.Service{ID: "api", Name: "api", Namespace: "default", Runtime: "container",
		Ports: []types.ServicePort{{Name: "http", Port: 8080}}}
	require.NoError(t, st.CreateService(ctx, svc))

	changed := time.Now()
	for _, inst := range []*types.Instance{
		{ID: "legacy", Name: "api-a", Namespace: "default", ServiceName: "api", Status: types.InstanceStatusRunning,
			Metadata: &types.InstanceMetadata{ContainerIP: "10.0.0.1"}},
		{ID: "ready", Name: "api-b", Namespace: "default", ServiceName: "api", Status: types.InstanceStatusRunning,
			Ready: true, ReadyChangedAt: &changed, Metadata: &types.InstanceMetadata{ContainerIP: "10.0.0.2"}},
		{ID: "unready", Name: "api-c", Namespace: "default", ServiceName: "api", Status: types.InstanceStatusRunning,
			Ready: false, ReadyChangedAt: &changed, Metadata: &types.InstanceMetadata{ContainerIP: "10.0.0.3"}},
	} {
		require.NoError(t, st.CreateInstance(ctx, inst))
	}

	c.republishService(ctx, svc)

	healthy := map[string]bool{}
	for _, ep := range pub.eps {
		healthy[ep.InstanceID] = ep.Healthy
	}
	assert.Equal(t, map[string]bool{"legacy": true, "ready": true, "unready": false}, healthy)
}
