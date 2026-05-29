package client

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

// Inlined instances in `rune get service -o yaml` used to serialize zero
// timestamps (0001-01-01T00:00:00Z) and an empty IP because the embedded
// converter dropped those fields. Lock in that they round-trip.
func TestEmbeddedInstance_TimestampsAndIPRoundTrip(t *testing.T) {
	created := time.Date(2026, 5, 28, 23, 49, 33, 0, time.UTC)
	updated := time.Date(2026, 5, 28, 23, 51, 47, 0, time.UTC)

	in := &types.Instance{
		ID:        "i-1",
		Name:      "api-0",
		Namespace: "prod",
		Status:    types.InstanceStatusRunning,
		IP:        "10.96.0.46",
		CreatedAt: created,
		UpdatedAt: updated,
	}

	out := embeddedInstanceFromProto(embeddedInstanceToProto(in))

	assert.Equal(t, "10.96.0.46", out.IP)
	assert.True(t, out.CreatedAt.Equal(created), "createdAt: got %s want %s", out.CreatedAt, created)
	assert.True(t, out.UpdatedAt.Equal(updated), "updatedAt: got %s want %s", out.UpdatedAt, updated)
}

// Zero timestamps must serialize as empty (not "0001-01-01...") and come
// back as zero, so the omitempty proto field stays clean.
func TestEmbeddedInstance_ZeroTimestampsStayEmpty(t *testing.T) {
	in := &types.Instance{ID: "i-2", Name: "api-1", Namespace: "prod"}

	proto := embeddedInstanceToProto(in)
	assert.Equal(t, "", proto.CreatedAt)
	assert.Equal(t, "", proto.UpdatedAt)

	out := embeddedInstanceFromProto(proto)
	assert.True(t, out.CreatedAt.IsZero())
	assert.True(t, out.UpdatedAt.IsZero())
}
