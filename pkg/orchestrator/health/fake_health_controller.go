package health

import (
	"context"
	"sync"

	"github.com/runestack/rune/pkg/types"
)

// FakeController is a lightweight test double for Controller
type FakeController struct {
	mu      sync.Mutex
	started bool
	stopped bool
	added   []struct {
		Service  *types.Service
		Instance *types.Instance
	}
	removed []string
}

func NewFakeController() *FakeController {
	return &FakeController{
		added: make([]struct {
			Service  *types.Service
			Instance *types.Instance
		}, 0),
		removed: make([]string, 0),
	}
}

func (f *FakeController) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *FakeController) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *FakeController) AddInstance(service *types.Service, instance *types.Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, struct {
		Service  *types.Service
		Instance *types.Instance
	}{service, instance})
	return nil
}

func (f *FakeController) RemoveInstance(instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, instanceID)
	return nil
}

func (f *FakeController) GetHealthStatus(ctx context.Context, instanceID string) (*types.InstanceHealthStatus, error) {
	// For tests, default to healthy
	return &types.InstanceHealthStatus{
		InstanceID: instanceID,
		Liveness:   true,
		Readiness:  true,
	}, nil
}

// Helper accessors for assertions
func (f *FakeController) AddedInstances() []struct {
	Service  *types.Service
	Instance *types.Instance
} {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]struct {
		Service  *types.Service
		Instance *types.Instance
	}, len(f.added))
	copy(out, f.added)
	return out
}

func (f *FakeController) RemovedInstanceIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.removed))
	copy(out, f.removed)
	return out
}
