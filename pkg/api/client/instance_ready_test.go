package client

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProtoToInstance_Readiness(t *testing.T) {
	c := &InstanceClient{logger: log.NewLogger()}
	base := func() *generated.Instance {
		return &generated.Instance{Id: "i", Status: generated.InstanceStatus_INSTANCE_STATUS_RUNNING,
			CreatedAt: "2026-10-24T00:00:00Z", UpdatedAt: "2026-10-24T00:00:00Z",
			Metadata: &generated.InstanceMetadata{}}
	}

	withdrawn := base()
	withdrawn.Ready, withdrawn.ReadyChangedAt = false, "2026-10-24T01:00:00Z"
	inst, err := c.protoToInstance(withdrawn)
	require.NoError(t, err)
	assert.False(t, inst.IsReady())

	inst, err = c.protoToInstance(base())
	require.NoError(t, err)
	assert.True(t, inst.IsReady(), "a server that sends no verdict reads as ready")
}

// The client watch emits MODIFIED only when instanceEquals differs, so a
// readiness flip must count as a change.
func TestInstanceEquals_ReadinessFlip(t *testing.T) {
	changed := time.Now()
	a := &types.Instance{ID: "i", Status: types.InstanceStatusRunning}
	b := &types.Instance{ID: "i", Status: types.InstanceStatusRunning, Ready: false, ReadyChangedAt: &changed}
	assert.False(t, instanceEquals(a, b))
}
