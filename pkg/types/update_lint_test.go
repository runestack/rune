package types

// RUNE-042 Phase 7: the advisory lint rules. The most important of these
// warns about a silent semantic change — a scale-1 service that never opted
// into anything now briefly runs two copies during an update.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func warnsFor(svc *Service) string {
	return strings.Join(updateStrategyWarnings(svc), "\n")
}

// THE important one: recreate-everything used to guarantee that a single
// replica never overlapped with its replacement. Rolling does not, and
// nothing else tells the operator.
func TestLintWarnings_ScaleOneRunsTwoCopies(t *testing.T) {
	worker := &Service{
		Name: "emailer", Runtime: RuntimeTypeContainer, Scale: 1,
	}
	w := warnsFor(worker)
	assert.Contains(t, w, "update-runs-two-copies")
	assert.Contains(t, w, "updateStrategy: recreate", "the warning must name the one-line fix")
	assert.Contains(t, w, "queue consumers", "and say who this typically bites")

	// The documented opt-out silences it.
	worker.UpdateStrategy = &UpdateStrategy{Type: UpdateRecreate}
	assert.Empty(t, warnsFor(worker), "an explicit recreate opts out of all of this")

	// Multi-replica services always overlapped; nothing changed for them.
	scaled := &Service{Name: "api", Runtime: RuntimeTypeContainer, Scale: 3}
	assert.NotContains(t, warnsFor(scaled), "update-runs-two-copies")
}

func TestLintWarnings_NeedsReadiness(t *testing.T) {
	exposed := &Service{
		Name: "api", Runtime: RuntimeTypeContainer, Scale: 3,
		Ports: []ServicePort{{Name: "http", Port: 8080}},
	}
	w := warnsFor(exposed)
	assert.Contains(t, w, "update-needs-readiness")
	assert.Contains(t, w, "container started", "the warning must explain what the gate actually is")

	exposed.Health = &HealthCheck{Readiness: &Probe{Type: "http", Path: "/healthz", Port: 8080}}
	assert.NotContains(t, warnsFor(exposed), "update-needs-readiness")

	// A service with no ports serves no traffic; readiness is moot.
	worker := &Service{Name: "worker", Runtime: RuntimeTypeContainer, Scale: 3}
	assert.NotContains(t, warnsFor(worker), "update-needs-readiness")
}

// Each exclusive-resource class must be named accurately — an operator
// reading "a hostPort" when the cause is a volume will look in the wrong place.
func TestLintWarnings_OneAtATimeNamesTheCause(t *testing.T) {
	vol := &Service{
		Name: "db", Runtime: RuntimeTypeContainer, Scale: 2,
		Volumes: []VolumeMount{{Name: "data", MountPath: "/data", ClaimTemplate: &VolumeClaimTemplate{}}},
	}
	assert.Contains(t, warnsFor(vol), "claimTemplate volume")

	hp := &Service{
		Name: "edge", Runtime: RuntimeTypeContainer, Scale: 2,
		Ports: []ServicePort{{Name: "http", Port: 80, HostPort: 8080}},
	}
	assert.Contains(t, warnsFor(hp), "hostPort")

	proc := &Service{Name: "agent", Runtime: RuntimeTypeProcess, Scale: 2}
	assert.Contains(t, warnsFor(proc), "process runtime")

	// At scale 1 the consequence is spelled out.
	proc.Scale = 1
	assert.Contains(t, warnsFor(proc), "brief gap")
}

// A plain multi-replica container service with a probe is the happy path and
// must produce no noise at all.
func TestLintWarnings_HealthyServiceIsQuiet(t *testing.T) {
	svc := &Service{
		Name: "api", Runtime: RuntimeTypeContainer, Scale: 3,
		Ports:  []ServicePort{{Name: "http", Port: 8080}},
		Health: &HealthCheck{Readiness: &Probe{Type: "http", Path: "/healthz", Port: 8080}},
	}
	assert.Empty(t, warnsFor(svc))
}

// Warnings must reach the cast-file surface, since that is where operators
// actually run lint.
func TestCastFile_LintWarningsSurfaceThroughTheFile(t *testing.T) {
	cf, err := ParseCastFileFromBytes([]byte(`
service:
  name: emailer
  image: app:v1
  scale: 1
`), "")
	require.NoError(t, err)
	warns := cf.LintWarnings()
	require.NotEmpty(t, warns)
	assert.Contains(t, strings.Join(warns, "\n"), "update-runs-two-copies")
	assert.Contains(t, strings.Join(warns, "\n"), "emailer", "the finding must name the service")
}
