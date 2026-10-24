package cmd

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

// A withdrawn instance is Running but not serving, so `rune get services`
// must not count it as ready.
func TestReadyInstanceCount_ExcludesWithdrawn(t *testing.T) {
	changed := time.Now()
	svc := &types.Service{Scale: 3, Instances: []types.Instance{
		{Status: types.InstanceStatusRunning},
		{Status: types.InstanceStatusRunning, Ready: false, ReadyChangedAt: &changed},
		{Status: types.InstanceStatusStarting},
	}}
	assert.Equal(t, 1, readyInstanceCount(svc))
}

// `rune status` READY counts only instances taking traffic.
func TestBucketInstances_ReadyExcludesWithdrawn(t *testing.T) {
	changed := time.Now()
	ready, total, states := bucketInstances([]*types.Instance{
		{ServiceName: "api", Status: types.InstanceStatusRunning},
		{ServiceName: "api", Status: types.InstanceStatusRunning, Ready: false, ReadyChangedAt: &changed},
		{ServiceName: "api", Status: types.InstanceStatusStarting},
	})
	assert.Equal(t, 1, ready["api"])
	assert.Equal(t, 3, total)
	assert.Equal(t, 2, states.Running, "a withdrawn instance is still Running")
}
