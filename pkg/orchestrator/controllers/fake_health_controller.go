package controllers

import (
	"context"
	"sync"

	"github.com/runestack/rune/pkg/types"
)

// FakeHealthController is a lightweight test double for HealthController
type FakeHealthController struct {
	mu      sync.Mutex
	started bool
	stopped bool
	added   []struct {
		Service  *types.Service
		Instance *types.Instance
	}
	removed []string
}

func NewFakeHealthController() *FakeHealthController {
	return &FakeHealthController{
		added: make([]struct {
			Service  *types.Service
			Instance *types.Instance
		}, 0),
		removed: make([]string, 0),
	}
}

func (f *FakeHealthController) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *FakeHealthController) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *FakeHealthController) AddInstance(service *types.Service, instance *types.Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, struct {
		Service  *types.Service
		Instance *types.Instance
	}{service, instance})
	return nil
}

func (f *FakeHealthController) RemoveInstance(instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, instanceID)
	return nil
}

func (f *FakeHealthController) GetHealthStatus(ctx context.Context, instanceID string) (*types.InstanceHealthStatus, error) {
	// For tests, default to healthy
	return &types.InstanceHealthStatus{
		InstanceID: instanceID,
		Liveness:   true,
		Readiness:  true,
	}, nil
}

// Helper accessors for assertions
func (f *FakeHealthController) AddedInstances() []struct {
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

func (f *FakeHealthController) RemovedInstanceIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.removed))
	copy(out, f.removed)
	return out
}
