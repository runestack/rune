package service

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstanceModelToProto_CarriesReadiness(t *testing.T) {
	s := NewInstanceService(nil, nil, log.NewLogger())
	changed := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)

	p, err := s.instanceModelToProto(&types.Instance{
		ID: "i-1", Status: types.InstanceStatusRunning, Metadata: &types.InstanceMetadata{},
		Ready: false, ReadyChangedAt: &changed,
	})
	require.NoError(t, err)
	assert.False(t, p.Ready)
	assert.Equal(t, "2026-10-24T01:00:00Z", p.ReadyChangedAt)

	p, err = s.instanceModelToProto(&types.Instance{ID: "i-2", Metadata: &types.InstanceMetadata{}})
	require.NoError(t, err)
	assert.Empty(t, p.ReadyChangedAt, "no verdict recorded stays empty, which clients read as ready")
}
