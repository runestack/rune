package service

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestMergeInstanceStatus_readinessGate(t *testing.T) {
	assert.Equal(t, types.InstanceStatusStarting,
		mergeInstanceStatus(types.InstanceStatusStarting, types.InstanceStatusRunning))
}

func TestMergeInstanceStatus_runningDrift(t *testing.T) {
	assert.Equal(t, types.InstanceStatusFailed,
		mergeInstanceStatus(types.InstanceStatusRunning, types.InstanceStatusFailed))
}

func TestMergeInstanceStatus_tombstone(t *testing.T) {
	assert.Equal(t, types.InstanceStatusFailed,
		mergeInstanceStatus(types.InstanceStatusFailed, types.InstanceStatusRunning))
}

func TestMergeInstanceStatus_agreement(t *testing.T) {
	assert.Equal(t, types.InstanceStatusRunning,
		mergeInstanceStatus(types.InstanceStatusRunning, types.InstanceStatusRunning))
}
