// Package instance — isolated table tests for the pure halves of
// classification (RUNE-311 Phase 2): preClassifyInstance and
// classifyObserved need no store, no runner, no controller.
package instance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

func TestPreClassifyInstance(t *testing.T) {
	created := time.Now()
	svc := &types.Service{ID: "svc-1"}

	tests := []struct {
		name      string
		inst      *types.Instance
		wantDone  bool
		wantClass CompatClass
	}{
		{
			name:      "foreign service is broken without touching the runner",
			inst:      &types.Instance{ServiceID: "other", Status: types.InstanceStatusRunning},
			wantDone:  true,
			wantClass: CompatBroken,
		},
		{
			name:      "stuck-in-create failed record holds its slot",
			inst:      &types.Instance{ServiceID: "svc-1", Status: types.InstanceStatusFailed},
			wantDone:  true,
			wantClass: CompatOK,
		},
		{
			name:      "stalled stuck-in-create record is held, never probed",
			inst:      &types.Instance{ServiceID: "svc-1", Status: types.InstanceStatusStalled},
			wantDone:  true,
			wantClass: CompatOK,
		},
		{
			name: "recorded terminal state is broken",
			inst: &types.Instance{ServiceID: "svc-1", Status: types.InstanceStatusExited,
				ContainerEverCreatedAt: &created},
			wantDone:  true,
			wantClass: CompatBroken,
		},
		{
			name: "running instance defers to observation",
			inst: &types.Instance{ServiceID: "svc-1", Status: types.InstanceStatusRunning,
				ContainerEverCreatedAt: &created},
			wantDone: false,
		},
		{
			name:      "foreign-service check precedes the stuck-in-create guard",
			inst:      &types.Instance{ServiceID: "other", Status: types.InstanceStatusFailed},
			wantDone:  true,
			wantClass: CompatBroken,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, done := preClassifyInstance(tt.inst, svc)
			assert.Equal(t, tt.wantDone, done)
			if done {
				assert.Equal(t, tt.wantClass, v.Class, "reason: %s", v.Reason)
			}
		})
	}
}

func TestClassifyObserved(t *testing.T) {
	logger := log.NewLogger()
	svc := &types.Service{
		ID:       "svc-1",
		Runtime:  "docker",
		Image:    "app:v2",
		Metadata: &types.ServiceMetadata{TemplateGeneration: 3},
	}
	inst := func(gen int64, image string) *types.Instance {
		return &types.Instance{
			ServiceID:   "svc-1",
			ContainerID: "cid-1",
			Metadata:    &types.InstanceMetadata{ServiceGeneration: gen, Image: image},
		}
	}

	tests := []struct {
		name      string
		inst      *types.Instance
		obs       instanceObservation
		wantClass CompatClass
	}{
		{
			name:      "observation failure is broken with the observed reason",
			inst:      inst(3, "app:v2"),
			obs:       instanceObservation{brokenReason: "instance not found in runner: gone"},
			wantClass: CompatBroken,
		},
		{
			name:      "observation failure outranks a stale template generation",
			inst:      inst(1, "app:v1"),
			obs:       instanceObservation{brokenReason: "failed to get runner: no runner"},
			wantClass: CompatBroken,
		},
		{
			name:      "terminal in runner is broken even when template is outdated",
			inst:      inst(1, "app:v1"),
			obs:       instanceObservation{status: types.InstanceStatusExited},
			wantClass: CompatBroken,
		},
		{
			name:      "older template generation is outdated",
			inst:      inst(1, "app:v2"),
			obs:       instanceObservation{status: types.InstanceStatusRunning},
			wantClass: CompatOutdated,
		},
		{
			name:      "current generation and image is ok",
			inst:      inst(3, "app:v2"),
			obs:       instanceObservation{status: types.InstanceStatusRunning},
			wantClass: CompatOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := classifyObserved(tt.inst, svc, tt.obs, logger)
			assert.Equal(t, tt.wantClass, v.Class, "reason: %s", v.Reason)
		})
	}
}
