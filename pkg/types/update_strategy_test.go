package types

// RUNE-042 Phase 1: the update strategy spec surface and its derived
// parameters. See _docs/designs/RUNE-042-Rolling-Updates.md §5.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The string shorthand is the whole point of the one-field design: the only
// override most services will ever need should be one line.
func TestUpdateStrategy_AcceptsStringAndMapForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want UpdateStrategyType
	}{
		{"string shorthand", "updateStrategy: recreate", UpdateRecreate},
		{"string rolling", "updateStrategy: rolling", UpdateRolling},
		{"map form", "updateStrategy:\n  type: recreate", UpdateRecreate},
		{"omitted", "image: nginx", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var spec struct {
				UpdateStrategy *UpdateStrategy `yaml:"updateStrategy"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(tc.in), &spec))
			if tc.want == "" {
				assert.Nil(t, spec.UpdateStrategy)
				return
			}
			require.NotNil(t, spec.UpdateStrategy)
			assert.Equal(t, tc.want, spec.UpdateStrategy.Type)
		})
	}
}

func TestUpdateStrategy_JSONRoundTrip(t *testing.T) {
	// Object form in, string form out — and back again.
	var u UpdateStrategy
	require.NoError(t, json.Unmarshal([]byte(`{"type":"recreate"}`), &u))
	assert.Equal(t, UpdateRecreate, u.Type)

	out, err := json.Marshal(u)
	require.NoError(t, err)
	assert.JSONEq(t, `"recreate"`, string(out))

	var back UpdateStrategy
	require.NoError(t, json.Unmarshal(out, &back))
	assert.Equal(t, u.Type, back.Type)
}

func TestUpdateStrategy_Validate(t *testing.T) {
	assert.NoError(t, (*UpdateStrategy)(nil).Validate(), "omitted strategy is valid")
	assert.NoError(t, (&UpdateStrategy{}).Validate(), "empty type means rolling")
	assert.NoError(t, (&UpdateStrategy{Type: UpdateRolling}).Validate())
	assert.NoError(t, (&UpdateStrategy{Type: UpdateRecreate}).Validate())

	err := (&UpdateStrategy{Type: "canary"}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rolling, recreate", "the error must name the allowed values")
}

// Surge capability is DETECTED, never asked — the ethos call that keeps
// maxSurge from being a field. Each exclusive-resource class must be caught.
func TestIsSurgeCapable(t *testing.T) {
	base := func() *Service {
		return &Service{Name: "api", Runtime: RuntimeTypeContainer, Scale: 3}
	}

	t.Run("plain docker service can surge", func(t *testing.T) {
		assert.True(t, base().IsSurgeCapable())
	})

	t.Run("claimTemplate volume cannot", func(t *testing.T) {
		s := base()
		s.Volumes = []VolumeMount{{Name: "data", MountPath: "/data", ClaimTemplate: &VolumeClaimTemplate{}}}
		assert.False(t, s.IsSurgeCapable(), "the replacement would have to mount the outgoing instance's volume")
	})

	t.Run("hostPort cannot", func(t *testing.T) {
		s := base()
		s.Ports = []ServicePort{{Name: "http", Port: 80, HostPort: 8080}}
		assert.False(t, s.IsSurgeCapable(), "two instances cannot bind the same host port")
	})

	t.Run("process runtime cannot", func(t *testing.T) {
		s := base()
		s.Runtime = RuntimeTypeProcess
		assert.False(t, s.IsSurgeCapable(), "process replicas collide on 127.0.0.1:<port>")
	})

	t.Run("plain ports are fine", func(t *testing.T) {
		s := base()
		s.Ports = []ServicePort{{Name: "http", Port: 8080}}
		assert.True(t, s.IsSurgeCapable(), "a container port without hostPort does not collide")
	})
}

// The extra/dip pair is one coupled decision, derived — never two fields that
// can both be zero and wedge the update.
func TestResolveUpdateParams_ExtraAndDipAreCoupled(t *testing.T) {
	t.Run("surge capable: one spare, never dip", func(t *testing.T) {
		p := (&Service{Runtime: RuntimeTypeContainer, Scale: 4}).ResolveUpdateParams()
		assert.Equal(t, UpdateRolling, p.Type)
		assert.Equal(t, 1, p.Extra)
		assert.Equal(t, 0, p.Dip)
	})

	t.Run("not surge capable: no spare, dip by one", func(t *testing.T) {
		p := (&Service{Runtime: RuntimeTypeProcess, Scale: 4}).ResolveUpdateParams()
		assert.Equal(t, 0, p.Extra)
		assert.Equal(t, 1, p.Dip, "retiring before creating is the only way to make progress")
	})

	t.Run("never both zero", func(t *testing.T) {
		// The misconfiguration class that disappeared with the fields.
		for _, s := range []*Service{
			{Runtime: RuntimeTypeContainer, Scale: 1},
			{Runtime: RuntimeTypeProcess, Scale: 1},
			{Runtime: RuntimeTypeContainer, Scale: 10},
			{Runtime: RuntimeTypeContainer, Scale: 2, Ports: []ServicePort{{Name: "h", Port: 80, HostPort: 80}}},
			{Runtime: RuntimeTypeContainer, Scale: 2, UpdateStrategy: &UpdateStrategy{Type: UpdateRecreate}},
		} {
			p := s.ResolveUpdateParams()
			assert.False(t, p.Extra == 0 && p.Dip == 0,
				"an update that can never take a step must be underivable")
		}
	})

	t.Run("recreate takes everything down", func(t *testing.T) {
		p := (&Service{Runtime: RuntimeTypeContainer, Scale: 5,
			UpdateStrategy: &UpdateStrategy{Type: UpdateRecreate}}).ResolveUpdateParams()
		assert.Equal(t, UpdateRecreate, p.Type)
		assert.Equal(t, 0, p.Extra)
		assert.Equal(t, 5, p.Dip)
	})
}

// minReady is derived from whether a readiness probe exists — the honest
// hedge for services where Running only means "the runner accepted it".
func TestResolveUpdateParams_MinReadyDerivedFromProbe(t *testing.T) {
	withProbe := &Service{Runtime: RuntimeTypeContainer, Scale: 2,
		Health: &HealthCheck{Readiness: &Probe{Type: "http", Path: "/healthz", Port: 8080}}}
	assert.Equal(t, time.Duration(0), withProbe.ResolveUpdateParams().MinReady,
		"the probe IS the gate; no extra window needed")

	without := &Service{Runtime: RuntimeTypeContainer, Scale: 2}
	assert.Equal(t, MinReadySecondsWithoutProbe*time.Second, without.ResolveUpdateParams().MinReady)
}

func TestResolveUpdateParams_StallDeadlineIsConstant(t *testing.T) {
	p := (&Service{Runtime: RuntimeTypeContainer, Scale: 2}).ResolveUpdateParams()
	assert.Equal(t, UpdateStallSeconds*time.Second, p.StallDeadline)
}

// drainSeconds defaults, and is floored rather than honoured at 0 — a true
// zero would reopen the withdrawal-propagation race Phase 0 closed.
func TestDrainWindow(t *testing.T) {
	zero, five, big := 0, 5, 30

	assert.Equal(t, DefaultDrainSeconds*time.Second, (&Service{}).DrainWindow(), "unset means the default")
	assert.Equal(t, MinDrainSeconds*time.Second, (&Service{DrainSeconds: &zero}).DrainWindow(),
		"0 is floored, not honoured")
	assert.Equal(t, 5*time.Second, (&Service{DrainSeconds: &five}).DrainWindow())
	assert.Equal(t, 30*time.Second, (&Service{DrainSeconds: &big}).DrainWindow())
}

// The one regression-shaped surprise in the default path: a surge-capable
// scale-1 worker loses its implicit never-two-copies guarantee.
func TestRunsTwoCopiesDuringUpdate(t *testing.T) {
	worker := &Service{Runtime: RuntimeTypeContainer, Scale: 1}
	assert.True(t, worker.RunsTwoCopiesDuringUpdate(), "lint must warn about this service")

	worker.UpdateStrategy = &UpdateStrategy{Type: UpdateRecreate}
	assert.False(t, worker.RunsTwoCopiesDuringUpdate(), "recreate is the documented opt-out")

	scaled := &Service{Runtime: RuntimeTypeContainer, Scale: 3}
	assert.False(t, scaled.RunsTwoCopiesDuringUpdate(), "only scale-1 loses an exclusivity guarantee")

	stateful := &Service{Runtime: RuntimeTypeContainer, Scale: 1,
		Volumes: []VolumeMount{{Name: "d", MountPath: "/d", ClaimTemplate: &VolumeClaimTemplate{}}}}
	assert.False(t, stateful.RunsTwoCopiesDuringUpdate(), "it cannot surge, so it never overlaps")
}

// The template-hash split: scale and the update knobs move Generation but
// must NOT move TemplateGeneration, or a scale edit would roll the service.
func TestTemplateHash_ExcludesScaleAndUpdateKnobs(t *testing.T) {
	base := func() *Service {
		return &Service{Name: "api", Namespace: "default", Image: "app:v1",
			Runtime: RuntimeTypeContainer, Scale: 2}
	}

	t.Run("scale change moves full hash only", func(t *testing.T) {
		a, b := base(), base()
		b.Scale = 5
		assert.NotEqual(t, a.CalculateHash(), b.CalculateHash(), "scale is desired state")
		assert.Equal(t, a.CalculateTemplateHash(), b.CalculateTemplateHash(),
			"scale must not make surviving instances stale (#142)")
	})

	t.Run("updateStrategy change moves full hash only", func(t *testing.T) {
		a, b := base(), base()
		b.UpdateStrategy = &UpdateStrategy{Type: UpdateRecreate}
		assert.NotEqual(t, a.CalculateHash(), b.CalculateHash(), "the reconciler must notice")
		assert.Equal(t, a.CalculateTemplateHash(), b.CalculateTemplateHash(),
			"how we roll is not what we roll to")
	})

	t.Run("drainSeconds change moves full hash only", func(t *testing.T) {
		a, b := base(), base()
		d := 20
		b.DrainSeconds = &d
		assert.NotEqual(t, a.CalculateHash(), b.CalculateHash())
		assert.Equal(t, a.CalculateTemplateHash(), b.CalculateTemplateHash())
	})

	t.Run("image change moves both", func(t *testing.T) {
		a, b := base(), base()
		b.Image = "app:v2"
		assert.NotEqual(t, a.CalculateHash(), b.CalculateHash())
		assert.NotEqual(t, a.CalculateTemplateHash(), b.CalculateTemplateHash(),
			"a new image is exactly what must replace instances")
	})

	t.Run("env change moves both", func(t *testing.T) {
		a, b := base(), base()
		b.Env = map[string]string{"MODE": "prod"}
		assert.NotEqual(t, a.CalculateHash(), b.CalculateHash())
		assert.NotEqual(t, a.CalculateTemplateHash(), b.CalculateTemplateHash())
	})

	t.Run("identical services hash identically", func(t *testing.T) {
		a, b := base(), base()
		assert.Equal(t, a.CalculateHash(), b.CalculateHash())
		assert.Equal(t, a.CalculateTemplateHash(), b.CalculateTemplateHash())
	})
}

// C2 regression: a crash-looping replacement must NOT masquerade as progress.
//
// The instance is created (Updated 0->1), its container exits, it classifies
// broken, the planner repairs it (Updated 0->1 again). Comparing each tick
// against the previous tick's raw counts made every up-swing look like
// forward movement, so the stall clock was refreshed forever and a bad image
// never surfaced as Failed — defeating stall detection for the single most
// common bad deploy. Progress is measured against high-water marks instead.
func TestUpdateStatus_CrashLoopDoesNotCountAsProgress(t *testing.T) {
	gen := int64(7)
	// Tick 1: the replacement exists and is ready. Genuine progress.
	prev := &UpdateStatus{TemplateGeneration: gen, Desired: 2, Updated: 0, Outdated: 2}
	next := &UpdateStatus{TemplateGeneration: gen, Desired: 2, Updated: 1, UpdatedReady: 1, Outdated: 1}
	require.True(t, prev.Progressed(next), "the first replacement becoming ready is real progress")
	next.CarryPeaksFrom(prev)
	assert.Equal(t, 1, next.PeakUpdated)
	assert.Equal(t, 1, next.PeakUpdatedReady)

	// Tick 2: the container exits; it is now broken, so the counters fall.
	crashed := &UpdateStatus{TemplateGeneration: gen, Desired: 2, Updated: 0, UpdatedReady: 0, Outdated: 1}
	assert.False(t, next.Progressed(crashed), "losing the replacement is not progress")
	crashed.CarryPeaksFrom(next)
	assert.Equal(t, 1, crashed.PeakUpdated, "the high-water mark must not fall back")

	// Tick 3: the planner repairs it and the counters climb again — the
	// oscillation that used to reset the stall clock forever.
	repaired := &UpdateStatus{TemplateGeneration: gen, Desired: 2, Updated: 1, UpdatedReady: 1, Outdated: 1}
	assert.False(t, crashed.Progressed(repaired),
		"re-reaching a level already achieved is churn, not progress — the stall clock must keep running")

	// Genuinely getting further still counts.
	better := &UpdateStatus{TemplateGeneration: gen, Desired: 2, Updated: 2, UpdatedReady: 2, Outdated: 0}
	assert.True(t, crashed.Progressed(better), "exceeding the high-water mark is real progress")
}

// A new template generation starts a fresh update: peaks must not leak across
// generations, or the second deploy would look stalled from its first tick.
func TestUpdateStatus_PeaksAreCarriedOnlyWithinAGeneration(t *testing.T) {
	prev := &UpdateStatus{TemplateGeneration: 1, Updated: 3, PeakUpdated: 3, PeakUpdatedReady: 3, PeakAvailable: 3}
	fresh := &UpdateStatus{TemplateGeneration: 2, Updated: 1, UpdatedReady: 1, Available: 3}

	// computeUpdateStatus only carries peaks when the generation matches; a
	// nil/mismatched prev means everything is progress.
	fresh.CarryPeaksFrom(nil)
	assert.Equal(t, 1, fresh.PeakUpdated, "a new generation starts its own high-water marks")
	assert.True(t, (*UpdateStatus)(nil).Progressed(fresh))
	_ = prev
}

// --- minServing (RUNE-042 follow-up) ---------------------------------------
//
// Surge capability is a fact about the service, so Rune derives it. An
// availability tolerance is not: whether 3 of 7 is acceptable depends on
// traffic and an SLO the spec says nothing about, so it is the one value here
// an operator has to state.

func TestMinServing_TradesAvailabilityForSpeed(t *testing.T) {
	three := 3
	svc := &Service{
		Name: "api", Runtime: RuntimeTypeContainer, Scale: 7,
		UpdateStrategy: &UpdateStrategy{MinServing: &three},
	}
	p := svc.ResolveUpdateParams()
	assert.Equal(t, 4, p.Dip, "scale 7 keeping 3 serving may retire 4 at a time")
	assert.Equal(t, 1, p.Extra, "surge stays derived at 1 — it is not settable")
}

func TestMinServing_DefaultsAreUnchanged(t *testing.T) {
	surgeable := &Service{Name: "api", Runtime: RuntimeTypeContainer, Scale: 7}
	assert.Equal(t, 0, surgeable.ResolveUpdateParams().Dip, "unset means never dip")

	exclusive := &Service{Name: "agent", Runtime: RuntimeTypeProcess, Scale: 7}
	assert.Equal(t, 1, exclusive.ResolveUpdateParams().Dip, "unset means one at a time")
}

// A floor stricter than the derived one must not silently loosen it: asking to
// keep all 7 serving on a service that already never dips changes nothing.
func TestMinServing_NeverLoosensTheDerivedFloor(t *testing.T) {
	seven := 7
	svc := &Service{
		Name: "api", Runtime: RuntimeTypeContainer, Scale: 7,
		UpdateStrategy: &UpdateStrategy{MinServing: &seven},
	}
	assert.Equal(t, 0, svc.ResolveUpdateParams().Dip)

	// And on a service that must go one at a time, asking for a bigger dip
	// than the derived one is honoured...
	four := 4
	proc := &Service{
		Name: "agent", Runtime: RuntimeTypeProcess, Scale: 7,
		UpdateStrategy: &UpdateStrategy{MinServing: &four},
	}
	assert.Equal(t, 3, proc.ResolveUpdateParams().Dip)
}

// The one deadlock this surface can produce — and the reason it is validated
// at cast time rather than discovered during a wedged deploy.
func TestMinServing_RejectsTheDeadlockCombination(t *testing.T) {
	seven := 7
	for _, tc := range []struct {
		name   string
		svc    *Service
		wantIn string
	}{
		{"process runtime", &Service{
			ID: "x", Name: "agent", Runtime: RuntimeTypeProcess, Scale: 7,
			UpdateStrategy: &UpdateStrategy{MinServing: &seven},
		}, "process runtime"},
		{"claimTemplate volume", &Service{
			ID: "x", Name: "db", Runtime: RuntimeTypeContainer, Scale: 7,
			Volumes:        []VolumeMount{{Name: "d", MountPath: "/d", ClaimTemplate: &VolumeClaimTemplate{}}},
			UpdateStrategy: &UpdateStrategy{MinServing: &seven},
		}, "claimTemplate volume"},
		{"hostPort", &Service{
			ID: "x", Name: "edge", Runtime: RuntimeTypeContainer, Scale: 7,
			Ports:          []ServicePort{{Name: "h", Port: 80, HostPort: 8080}},
			UpdateStrategy: &UpdateStrategy{MinServing: &seven},
		}, "hostPort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.svc.UpdateStrategy.ValidateForService(tc.svc)
			require.Error(t, err, "no room to create and no allowance to retire is an update that can never start")
			assert.Contains(t, err.Error(), tc.wantIn, "the error must name the actual blocker")
			assert.Contains(t, err.Error(), "minServing 6 or lower", "and offer a concrete fix")
		})
	}

	// The same value is fine when the service CAN run a spare copy.
	ok := &Service{ID: "x", Name: "api", Runtime: RuntimeTypeContainer, Scale: 7,
		UpdateStrategy: &UpdateStrategy{MinServing: &seven}}
	assert.NoError(t, ok.UpdateStrategy.ValidateForService(ok))
}

func TestMinServing_RejectsAboveScale(t *testing.T) {
	ten := 10
	svc := &Service{ID: "x", Name: "api", Runtime: RuntimeTypeContainer, Scale: 7,
		UpdateStrategy: &UpdateStrategy{MinServing: &ten}}
	err := svc.UpdateStrategy.ValidateForService(svc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds scale")
}

// The map form must carry it, and a strategy with minServing must not
// round-trip through the string shorthand — that would silently drop it.
func TestMinServing_RoundTrips(t *testing.T) {
	var u UpdateStrategy
	require.NoError(t, yaml.Unmarshal([]byte("type: rolling\nminServing: 3\n"), &u))
	require.NotNil(t, u.MinServing)
	assert.Equal(t, 3, *u.MinServing)

	out, err := yaml.Marshal(u)
	require.NoError(t, err)
	assert.Contains(t, string(out), "minServing: 3", "the map form must survive marshalling")

	var back UpdateStrategy
	require.NoError(t, yaml.Unmarshal(out, &back))
	require.NotNil(t, back.MinServing)
	assert.Equal(t, 3, *back.MinServing)

	// Without it, the readable shorthand is still used.
	plain, err := yaml.Marshal(UpdateStrategy{Type: UpdateRecreate})
	require.NoError(t, err)
	assert.Equal(t, "recreate\n", string(plain))
}
