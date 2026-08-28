package releasectl

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/release"
	"github.com/stretchr/testify/require"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

func genService(image string, scale int) *types.Service {
	return &types.Service{
		Name: "web", Namespace: "default", Image: image, Scale: scale,
		Metadata: &types.ServiceMetadata{},
	}
}

// A rendered service starts at zero, so writing it through unchanged
// resets both counters — which is what silently disabled the template
// check for every cast-managed service.
func TestCarryGenerations_DoesNotResetStoredCounters(t *testing.T) {
	stored := genService("nginx:1", 1)
	stored.Metadata.Generation = 7
	stored.Metadata.TemplateGeneration = 5
	stored.Metadata.ObservedGeneration = 7

	rendered := genService("nginx:1", 1)
	carryGenerations(stored, rendered)

	assert.EqualValues(t, 7, rendered.Metadata.Generation, "an unchanged spec is not a new generation")
	assert.EqualValues(t, 5, rendered.Metadata.TemplateGeneration)
	assert.EqualValues(t, 7, rendered.Metadata.ObservedGeneration)
}

// A template change has to move both, or nothing replaces the instances.
func TestCarryGenerations_TemplateChangeMovesBoth(t *testing.T) {
	stored := genService("nginx:1", 1)
	stored.Metadata.Generation = 7
	stored.Metadata.TemplateGeneration = 5

	rendered := genService("nginx:2", 1)
	carryGenerations(stored, rendered)

	assert.EqualValues(t, 8, rendered.Metadata.Generation)
	assert.EqualValues(t, 8, rendered.Metadata.TemplateGeneration)
}

// A scale edit is a new generation and NOT a new template: replacing every
// instance because the replica count moved is the bug the template/full
// split exists to prevent.
func TestCarryGenerations_ScaleChangeLeavesTheTemplate(t *testing.T) {
	stored := genService("nginx:1", 1)
	stored.Metadata.Generation = 7
	stored.Metadata.TemplateGeneration = 5

	rendered := genService("nginx:1", 3)
	carryGenerations(stored, rendered)

	assert.EqualValues(t, 8, rendered.Metadata.Generation)
	assert.EqualValues(t, 5, rendered.Metadata.TemplateGeneration, "scale must not replace instances")
}

// A service that has been through the resetting behaviour has stored
// zeros. Leaving them at zero would keep the counter dead forever, so the
// first apply after this lands adopts one.
func TestCarryGenerations_StoredZeroBecomesOne(t *testing.T) {
	stored := genService("nginx:1", 1)
	rendered := genService("nginx:1", 1)

	carryGenerations(stored, rendered)

	assert.EqualValues(t, 1, rendered.Metadata.Generation)
	assert.EqualValues(t, 1, rendered.Metadata.TemplateGeneration)
}

func TestCarryGenerations_NilSafe(t *testing.T) {
	rendered := genService("nginx:1", 1)
	carryGenerations(nil, rendered)
	carryGenerations(&types.Service{}, rendered)
	assert.EqualValues(t, 0, rendered.Metadata.Generation)
}

// Through Apply, not the helper: a correct helper nobody calls carries
// nothing, and the whole bug was that the apply path wrote a rendered
// service straight over the stored counters.
func TestApply_CarriesTheCountersOntoTheStoredService(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := genService("nginx:1", 1)
	stored.Metadata.Generation = 7
	stored.Metadata.TemplateGeneration = 5
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): genService("nginx:2", 1),
	}}}

	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.EqualValues(t, 8, after.Metadata.Generation,
		"a rendered service starts at zero; without the carry this is 0")
	assert.EqualValues(t, 8, after.Metadata.TemplateGeneration,
		"the image changed, so instances must be replaced")
}
