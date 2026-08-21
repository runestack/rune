package types

// RUNE-042 Phase 1: the fields must be ACCEPTED by cast's hand-maintained
// allowlist and survive into the Service, not merely parse into the struct.
// TestValidServiceFieldsMatchesSpec guards the allowlist mechanically; these
// go through the real castfile parser so the whole path is exercised.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func castOneService(t *testing.T, yamlDoc string) (*ServiceSpec, error) {
	t.Helper()
	cf, err := ParseCastFileFromBytes([]byte(yamlDoc), "")
	if err != nil {
		return nil, err
	}
	require.Len(t, cf.Services, 1, "fixture must declare exactly one service")
	spec := cf.Services[0]
	return &spec, spec.Validate()
}

func TestCast_AcceptsUpdateStrategyAndDrainSeconds(t *testing.T) {
	spec, err := castOneService(t, `
service:
  name: api
  image: app:v1
  scale: 3
  drainSeconds: 10
  updateStrategy: recreate
`)
	require.NoError(t, err, "cast must accept updateStrategy + drainSeconds")
	require.NotNil(t, spec.UpdateStrategy)
	assert.Equal(t, UpdateRecreate, spec.UpdateStrategy.Type)
	require.NotNil(t, spec.DrainSeconds)
	assert.Equal(t, 10, *spec.DrainSeconds)

	svc, err := spec.ToService()
	require.NoError(t, err)
	require.NotNil(t, svc.UpdateStrategy, "ToService must carry the strategy through")
	assert.Equal(t, UpdateRecreate, svc.UpdateStrategy.Type)
	require.NotNil(t, svc.DrainSeconds)
	assert.Equal(t, 10, *svc.DrainSeconds)
	assert.Equal(t, 0, svc.ResolveUpdateParams().Extra, "recreate never surges")
}

func TestCast_AcceptsUpdateStrategyMapForm(t *testing.T) {
	spec, err := castOneService(t, `
service:
  name: api
  image: app:v1
  updateStrategy:
    type: rolling
`)
	require.NoError(t, err)
	require.NotNil(t, spec.UpdateStrategy)
	assert.Equal(t, UpdateRolling, spec.UpdateStrategy.Type)
}

func TestCast_RejectsUnknownStrategyType(t *testing.T) {
	_, err := castOneService(t, `
service:
  name: api
  image: app:v1
  updateStrategy: canary
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rolling, recreate", "the error must name the allowed values")
}

func TestCast_RejectsOutOfRangeDrainSeconds(t *testing.T) {
	_, err := castOneService(t, `
service:
  name: api
  image: app:v1
  drainSeconds: 99999
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drainSeconds")
}

// A typo inside the map form must fail the cast rather than silently leaving
// the service on the default strategy.
func TestCast_RejectsUnknownFieldInsideUpdateStrategy(t *testing.T) {
	_, err := castOneService(t, `
service:
  name: api
  image: app:v1
  updateStrategy:
    typ: recreate
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "typ")
}
