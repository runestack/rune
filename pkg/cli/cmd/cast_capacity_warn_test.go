package cmd

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func svcWithMem(name, request string) *types.Service {
	return &types.Service{Name: name, Resources: types.Resources{
		Memory: types.ResourceLimit{Request: request},
	}}
}

// The scenario this exists for: a pool of nominal 24GB nodes, a service
// asking for 24Gi. It fits neither — 24Gi is 25.8GB before any reserve —
// and the operator should learn that at cast time rather than from an
// instance that never leaves Pending.
func TestCapacityWarnings_RequestExceedsLargestNode(t *testing.T) {
	alloc := &nodeAllocatable{millicores: 8000, memBytes: 21_600_000_000}

	warns := capacityWarnings([]*types.Service{svcWithMem("app", "24Gi")}, alloc)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "app requests")
	assert.Contains(t, warns[0], "the largest node can offer")
	assert.Contains(t, warns[0], "kernel and agent",
		"the message must explain why the offer is less than the machine's size")
}

func TestCapacityWarnings_RequestThatFitsIsSilent(t *testing.T) {
	alloc := &nodeAllocatable{millicores: 8000, memBytes: 21_600_000_000}
	assert.Empty(t, capacityWarnings([]*types.Service{svcWithMem("app", "16Gi")}, alloc))
}

// Comparing against CAPACITY rather than allocatable is the bug this
// guards: 20Gi fits a 22.4Gi machine and does not fit what the machine
// can offer.
func TestCapacityWarnings_ComparesAgainstAllocatableNotCapacity(t *testing.T) {
	// A nominal 24GB node: ~22.4Gi total, ~20.1Gi allocatable.
	alloc := &nodeAllocatable{memBytes: 21_600_000_000}
	warns := capacityWarnings([]*types.Service{svcWithMem("app", "21Gi")}, alloc)
	require.Len(t, warns, 1, "a request between allocatable and total must warn")
}

func TestCapacityWarnings_CPU(t *testing.T) {
	alloc := &nodeAllocatable{millicores: 7800}
	svc := &types.Service{Name: "app", Resources: types.Resources{
		CPU: types.ResourceLimit{Request: "8"},
	}}
	warns := capacityWarnings([]*types.Service{svc}, alloc)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "requests 8 CPU")
	assert.Contains(t, warns[0], "7800m")
}

// Unknown capacity must be silent, never a warning and never a refusal:
// an older server, a denied call, or a node that has not reported are all
// reasons to say nothing rather than to scold.
func TestCapacityWarnings_UnknownCapacityIsSilent(t *testing.T) {
	assert.Nil(t, capacityWarnings([]*types.Service{svcWithMem("app", "999Gi")}, nil))
}

// A request Rune cannot parse is Validate's problem, not this one —
// warning about it here would double up on an error the user already got.
func TestCapacityWarnings_UnparseableRequestIsSilent(t *testing.T) {
	alloc := &nodeAllocatable{memBytes: 1 << 30}
	assert.Empty(t, capacityWarnings([]*types.Service{svcWithMem("app", "24gb")}, alloc))
}

func TestCapacityWarnings_NoRequestIsSilent(t *testing.T) {
	alloc := &nodeAllocatable{millicores: 8000, memBytes: 21_600_000_000}
	assert.Empty(t, capacityWarnings([]*types.Service{{Name: "app"}}, alloc))
}

func TestFormatMillicores(t *testing.T) {
	assert.Equal(t, "8", formatMillicores(8000))
	assert.Equal(t, "7800m", formatMillicores(7800))
}
