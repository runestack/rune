package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// FakeScalingController is a lightweight test double for ScalingController
type FakeScalingController struct {
	mu         sync.Mutex
	started    bool
	stopped    bool
	createdOps []*types.ScalingOperation
	activeOps  map[string]*types.ScalingOperation // key: namespace/name
}

func NewFakeScalingController() *FakeScalingController {
	return &FakeScalingController{
		createdOps: make([]*types.ScalingOperation, 0),
		activeOps:  make(map[string]*types.ScalingOperation),
	}
}

func (f *FakeScalingController) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *FakeScalingController) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
}

func (f *FakeScalingController) CreateScalingOperation(ctx context.Context, service *types.Service, params types.ScalingOperationParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := fmt.Sprintf("fake-scale-%s-%d", service.Name, time.Now().UnixNano())
	op := &types.ScalingOperation{
		ID:           id,
		Namespace:    service.Namespace,
		ServiceName:  service.Name,
		CurrentScale: params.CurrentScale,
		TargetScale:  params.TargetScale,
		StepSize:     params.StepSize,
		Interval:     params.IntervalSeconds,
		StartTime:    time.Now(),
		Status:       types.ScalingOperationStatusInProgress,
		Mode:         types.ScalingModeImmediate,
	}
	if params.IsGradual {
		op.Mode = types.ScalingModeGradual
	}

	f.createdOps = append(f.createdOps, op)
	key := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	f.activeOps[key] = op
	return nil
}

func (f *FakeScalingController) GetActiveOperation(ctx context.Context, namespace, serviceName string) (*types.ScalingOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fmt.Sprintf("%s/%s", namespace, serviceName)
	if op, ok := f.activeOps[key]; ok && op.Status == types.ScalingOperationStatusInProgress {
		return op, nil
	}
	return nil, nil
}

// Helpers for tests
func (f *FakeScalingController) CreatedOperations() []*types.ScalingOperation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*types.ScalingOperation, len(f.createdOps))
	copy(out, f.createdOps)
	return out
}

func (f *FakeScalingController) CompleteOperation(namespace, serviceName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fmt.Sprintf("%s/%s", namespace, serviceName)
	if op, ok := f.activeOps[key]; ok {
		op.Status = types.ScalingOperationStatusCompleted
		op.EndTime = time.Now()
		delete(f.activeOps, key)
	}
}
