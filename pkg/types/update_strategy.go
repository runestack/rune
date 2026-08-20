package types

// RUNE-042 Phase 1: the update strategy spec surface and its derived
// parameters. See _docs/designs/RUNE-042-Rolling-Updates.md §5.
//
// The surface is deliberately two knobs — `updateStrategy` (one field) and a
// service-level `drainSeconds`. Everything else a rolling update needs (how
// many extra copies may exist, how far below scale it may dip, how long a
// replacement must hold Running, when a stuck update is declared stalled) is
// DERIVED per service rather than configured. An earlier draft exposed all of
// them as fields, K8s-style; each one had a defaults rule that computed the
// right answer from information Rune already has, which is the proof they
// never needed to be fields. Fields can be added later if real demand
// appears; they can never be removed.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// UpdateStrategyType selects how outdated instances are replaced.
type UpdateStrategyType string

const (
	// UpdateRolling replaces outdated instances incrementally: create a
	// replacement, wait until it serves, retire the old one. The default.
	UpdateRolling UpdateStrategyType = "rolling"

	// UpdateRecreate tears every outdated instance down before creating any
	// replacement — the pre-RUNE-042 behaviour, kept as an explicit opt-in
	// for services that genuinely cannot run two copies: an exclusive
	// external lock, a single-writer migration, licence-seat limits, or a
	// scale-1 queue consumer that must never overlap with its replacement.
	UpdateRecreate UpdateStrategyType = "recreate"
)

// Update timing constants. These are the derived values that were once spec
// fields; they live here so the derivation is readable in one place.
const (
	// DefaultDrainSeconds is how long an instance keeps serving in-flight
	// work after leaving the dataplane endpoint set, before SIGTERM.
	DefaultDrainSeconds = 5

	// MinDrainSeconds is the floor applied even when drainSeconds is 0.
	// Endpoint withdrawal propagates asynchronously (publish → OrderedLog →
	// agent watch → proxy cache); dropping to a true zero would put SIGTERM
	// immediately after the publish and reopen the race the drain exists to
	// close.
	MinDrainSeconds = 1

	// MaxDrainSeconds bounds the field so a typo cannot wedge a teardown.
	MaxDrainSeconds = 3600

	// MinReadySecondsWithoutProbe is the minimum-ready window for a service
	// with no readiness probe. Without a probe, Running only means "the
	// runner accepted the container", so an update would otherwise advance
	// on a container that has not bound its port yet. With a probe the
	// window is 0 — the probe IS the gate. `rune lint` pushes operators
	// toward the probe rather than toward tuning this.
	MinReadySecondsWithoutProbe = 5

	// UpdateStallSeconds is how long an update may make no progress before
	// it is declared stalled. Long enough for a slow image pull on a small
	// box. A constant rather than a field: per-service tuning of this mostly
	// existed to be mis-set against the release verify timeout.
	UpdateStallSeconds = 600
)

// UpdateStrategy is how a service replaces its instances when the template
// changes. One field, on purpose — the YAML map form exists so a future knob
// has somewhere to live without breaking anyone, while the string shorthand
// (`updateStrategy: recreate`) covers the only override most services will
// ever need.
type UpdateStrategy struct {
	Type UpdateStrategyType `json:"type,omitempty" yaml:"type,omitempty"`

	// MinServing is the fewest replicas that must keep serving while an
	// update runs. Set it to trade availability for speed: a scale-7 service
	// with MinServing 3 may retire four replicas at once instead of one, so
	// the update takes about two steps instead of seven.
	//
	// It states the operator's availability tolerance, which is the one thing
	// here Rune genuinely cannot derive — surge capability follows from the
	// spec (volumes, ports, runtime), but whether 3 of 7 is acceptable
	// depends on traffic and an SLO that the spec says nothing about.
	//
	// Lives inside UpdateStrategy rather than on the service because it means
	// nothing outside an update: a scale-down still converges to the desired
	// scale, and a liveness restart still replaces a dead container.
	// (DrainSeconds is on the service for the mirror-image reason — it
	// governs every teardown.)
	//
	// nil means the default: the full scale for a service that can run a
	// spare copy (never dip), one less for a service that cannot.
	MinServing *int `json:"minServing,omitempty" yaml:"minServing,omitempty"`
}

// updateStrategyAlias avoids infinite recursion in the custom (un)marshallers.
type updateStrategyAlias struct {
	Type       UpdateStrategyType `json:"type,omitempty" yaml:"type,omitempty"`
	MinServing *int               `json:"minServing,omitempty" yaml:"minServing,omitempty"`
}

// UnmarshalYAML accepts either the string shorthand (`updateStrategy: recreate`)
// or the map form (`updateStrategy: {type: recreate}`).
func (u *UpdateStrategy) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		u.Type = UpdateStrategyType(strings.TrimSpace(s))
		return nil
	}
	var alias updateStrategyAlias
	if err := value.Decode(&alias); err != nil {
		return err
	}
	u.Type = alias.Type
	u.MinServing = alias.MinServing
	return nil
}

// MarshalYAML emits the string shorthand when Type is all that is set, and the
// map form otherwise — emitting the shorthand with a MinServing present would
// silently drop it on round-trip.
func (u UpdateStrategy) MarshalYAML() (interface{}, error) {
	if u.MinServing == nil {
		return string(u.Type), nil
	}
	return updateStrategyAlias(u), nil
}

// UnmarshalJSON mirrors UnmarshalYAML: a bare string or an object.
func (u *UpdateStrategy) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		u.Type = UpdateStrategyType(strings.TrimSpace(s))
		return nil
	}
	var alias updateStrategyAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	u.Type = alias.Type
	u.MinServing = alias.MinServing
	return nil
}

// MarshalJSON emits the string shorthand, matching MarshalYAML — and, for the
// same round-trip reason, the object form once MinServing is set.
func (u UpdateStrategy) MarshalJSON() ([]byte, error) {
	if u.MinServing == nil {
		return json.Marshal(string(u.Type))
	}
	return json.Marshal(updateStrategyAlias(u))
}

// Validate checks the strategy type. An empty type means "rolling".
func (u *UpdateStrategy) Validate() error {
	if u == nil {
		return nil
	}
	switch u.Type {
	case "", UpdateRolling, UpdateRecreate:
	default:
		return NewValidationError(fmt.Sprintf(
			"invalid updateStrategy %q (allowed: rolling, recreate)", u.Type))
	}
	if u.MinServing != nil && *u.MinServing < 0 {
		return NewValidationError("updateStrategy.minServing cannot be negative")
	}
	return nil
}

// ValidateForService checks the strategy against the service it belongs to.
// Separate from Validate because these rules need the scale and the service's
// surge capability, neither of which the strategy knows on its own.
func (u *UpdateStrategy) ValidateForService(svc *Service) error {
	if u == nil || u.MinServing == nil || svc == nil {
		return nil
	}
	minServing := *u.MinServing

	if minServing > svc.Scale {
		return NewValidationError(fmt.Sprintf(
			"updateStrategy.minServing (%d) exceeds scale (%d): an update could never satisfy it",
			minServing, svc.Scale))
	}

	// The one deadlock this surface can produce, and the reason it is worth
	// validating rather than discovering during a wedged deploy: a service
	// that cannot run a spare copy has no room to create a replacement, so if
	// it also may not drop a replica, no update can ever take a step.
	if minServing == svc.Scale && !svc.IsSurgeCapable() {
		return NewValidationError(fmt.Sprintf(
			"updateStrategy.minServing (%d) equals scale, but this service cannot run two copies of a "+
				"replica (%s), so an update could never start: it has no room to create a replacement "+
				"and no allowance to retire one. Use minServing %d or lower, or updateStrategy: recreate",
			minServing, surgeBlocker(svc), svc.Scale-1))
	}
	return nil
}

// surgeBlocker names the exclusive resource that stops a service running two
// copies of a replica, so the error points at the actual cause.
func surgeBlocker(svc *Service) string {
	for i := range svc.Volumes {
		if svc.Volumes[i].ClaimTemplate != nil {
			return "a per-replica claimTemplate volume"
		}
	}
	if svc.Runtime == RuntimeTypeProcess {
		return "the process runtime"
	}
	for i := range svc.Ports {
		if svc.Ports[i].HostPort != 0 {
			return "a hostPort"
		}
	}
	return "an exclusive resource"
}

// UpdateParams are the resolved, per-service values the update planner runs
// on. Derived at read time and never persisted, so changing a derivation rule
// does not require rewriting every stored service record.
type UpdateParams struct {
	// Type is the effective strategy ("rolling" when unset).
	Type UpdateStrategyType

	// Extra is how many instances may exist ABOVE the desired scale during
	// an update — 1 when the service can run two copies of a replica, else 0.
	Extra int

	// Dip is how many instances may be BELOW the desired scale — 0 when
	// Extra is 1 (never dip if a spare copy is possible), else 1 (going down
	// by one is the only way to make progress). Extra and Dip are one
	// coupled decision, which is why they are derived together: exposing
	// them as separate fields is what creates the "both zero, so the update
	// can never take a step" misconfiguration.
	Dip int

	// MinReady is how long an instance must hold Running before it counts as
	// serving.
	MinReady time.Duration

	// StallDeadline is how long the update may make no progress before it is
	// declared stalled.
	StallDeadline time.Duration

	// Drain is the shutdown grace applied to every teardown of this
	// service's instances.
	Drain time.Duration
}

// IsSurgeCapable reports whether a second copy of a replica can run
// concurrently with the one it replaces. False means the update must retire
// before it creates, one replica at a time.
func (s *Service) IsSurgeCapable() bool {
	if s == nil {
		return false
	}
	// Per-replica claimTemplate volumes: the replacement would have to mount
	// the volume the outgoing instance still holds.
	for i := range s.Volumes {
		if s.Volumes[i].ClaimTemplate != nil {
			return false
		}
	}
	// Process-runner instances are dialed at 127.0.0.1:<port> on the host and
	// the runner exposes no per-instance IP, so two copies of a process
	// replica collide on the same port.
	if s.Runtime == RuntimeTypeProcess {
		return false
	}
	// hostPort binds a fixed port on the host; two instances cannot share it.
	for i := range s.Ports {
		if s.Ports[i].HostPort != 0 {
			return false
		}
	}
	return true
}

// HasReadinessProbe reports whether the service defines a readiness probe —
// the signal that makes Status=Running mean "actually serving".
func (s *Service) HasReadinessProbe() bool {
	return s != nil && s.Health != nil && s.Health.Readiness != nil
}

// DrainWindow is the shutdown grace for this service's instances: the pause
// between withdrawing an instance from the dataplane and stopping its
// container. Defaults to DefaultDrainSeconds and is floored at
// MinDrainSeconds — a true zero would reopen the withdrawal-propagation race.
func (s *Service) DrainWindow() time.Duration {
	secs := DefaultDrainSeconds
	if s != nil && s.DrainSeconds != nil {
		secs = *s.DrainSeconds
	}
	if secs < MinDrainSeconds {
		secs = MinDrainSeconds
	}
	return time.Duration(secs) * time.Second
}

// ResolveUpdateParams derives the update parameters for this service. Pure
// and cheap: call it wherever the values are needed rather than caching.
func (s *Service) ResolveUpdateParams() UpdateParams {
	p := UpdateParams{
		Type:          UpdateRolling,
		StallDeadline: UpdateStallSeconds * time.Second,
		Drain:         s.DrainWindow(),
	}
	if s != nil && s.UpdateStrategy != nil && s.UpdateStrategy.Type != "" {
		p.Type = s.UpdateStrategy.Type
	}

	// Surge capability decides the extra/dip pair together.
	//
	// Surge stays at 1 by design and is not settable. On 1-3 boxes spare
	// capacity is the scarce resource — a box is typically sized for about
	// sum(scale) — so the speed knob Rune offers is dipping, which is free,
	// rather than surging, which costs exactly what is short. (K8s exposes
	// maxSurge because a cluster can usually find another node's worth of
	// room.)
	if s.IsSurgeCapable() {
		p.Extra, p.Dip = 1, 0
	} else {
		p.Extra, p.Dip = 0, 1
	}

	// An explicit availability floor overrides the derived dip. This is the
	// one value here that is genuinely operator knowledge rather than a fact
	// about the service.
	if s != nil && s.UpdateStrategy != nil && s.UpdateStrategy.MinServing != nil {
		if dip := s.Scale - *s.UpdateStrategy.MinServing; dip > p.Dip {
			p.Dip = dip
		}
	}

	// Recreate takes everything down before creating: no spare copy, and the
	// dip is the whole service.
	if p.Type == UpdateRecreate {
		p.Extra = 0
		p.Dip = s.Scale
		if p.Dip < 1 {
			p.Dip = 1
		}
	}

	if !s.HasReadinessProbe() {
		p.MinReady = MinReadySecondsWithoutProbe * time.Second
	}
	return p
}

// RunsTwoCopiesDuringUpdate reports the case `rune lint` warns about: a
// surge-capable single-replica service silently loses the never-two-copies
// guarantee that recreate-everything used to give it for free. Queue
// consumers and cron-style workers may depend on that exclusivity.
func (s *Service) RunsTwoCopiesDuringUpdate() bool {
	if s == nil || s.Scale != 1 || !s.IsSurgeCapable() {
		return false
	}
	return s.ResolveUpdateParams().Type == UpdateRolling
}

// UpdateStatus is the progress of an in-flight update: what `rune status`,
// `describe`, the dashboard and the CLI spinner narrate, and what stall
// detection keys off. nil on Service means no update is running.
//
// The CLI deliberately renders only two of these fractions — replaced and
// serving — in operator words; the rest are here for the dashboard and for
// scripting.
type UpdateStatus struct {
	// TemplateGeneration is the generation being updated to.
	TemplateGeneration int64 `json:"templateGeneration,omitempty" yaml:"templateGeneration,omitempty"`

	// Desired is the service's scale.
	Desired int `json:"desired" yaml:"desired"`

	// Updated is how many instances are at TemplateGeneration.
	Updated int `json:"updated" yaml:"updated"`

	// UpdatedReady is how many of those are Running.
	UpdatedReady int `json:"updatedReady" yaml:"updatedReady"`

	// Available is how many instances are serving right now, any generation.
	Available int `json:"available" yaml:"available"`

	// Outdated is how many instances are still on an older template.
	Outdated int `json:"outdated" yaml:"outdated"`

	StartedAt time.Time `json:"startedAt" yaml:"startedAt"`

	// LastProgressAt is when Updated/UpdatedReady/Available last increased.
	// Persisted so a runed restart mid-update does not reset the stall clock
	// — the same reasoning that made ObservedGeneration a persisted field.
	LastProgressAt time.Time `json:"lastProgressAt" yaml:"lastProgressAt"`

	// Message is the planner's one-sentence reason.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	// Peak* are high-water marks for this template generation: the best
	// each counter has EVER reached, never decreasing while the generation
	// stands. Progress is measured against these rather than against the
	// previous tick's raw counts.
	//
	// Comparing against the previous tick is what a crash-looping
	// replacement defeats: the instance is created (Updated 0->1), its
	// container exits, it classifies broken, the planner repairs it
	// (Updated 0->1 again). Every up-swing of that cycle looks like
	// progress, so LastProgressAt is refreshed forever and the update never
	// stalls — precisely for the most common bad deploy there is. Against a
	// high-water mark the oscillation never exceeds its own peak, so the
	// stall deadline fires as intended.
	PeakUpdated      int `json:"peakUpdated,omitempty" yaml:"peakUpdated,omitempty"`
	PeakUpdatedReady int `json:"peakUpdatedReady,omitempty" yaml:"peakUpdatedReady,omitempty"`
	PeakAvailable    int `json:"peakAvailable,omitempty" yaml:"peakAvailable,omitempty"`
}

// Progressed reports whether next represents forward movement past everything
// this update has previously achieved. Used for stall detection: an update
// that stops progressing for longer than the stall deadline is declared
// stalled, which is what lets `cast --atomic` roll a wedged update back.
//
// The comparison is against u's HIGH-WATER MARKS, not its current counts, so
// an oscillating counter (the crash-loop-and-repair cycle) cannot masquerade
// as progress. See the Peak* fields.
func (u *UpdateStatus) Progressed(next *UpdateStatus) bool {
	if u == nil || next == nil {
		return true
	}
	return next.UpdatedReady > u.PeakUpdatedReady ||
		next.Available > u.PeakAvailable ||
		next.Updated > u.PeakUpdated
}

// CarryPeaksFrom seeds next's high-water marks from prev and raises them to
// cover next's own counts. Call once per tick, before Progressed.
func (next *UpdateStatus) CarryPeaksFrom(prev *UpdateStatus) {
	if prev != nil {
		next.PeakUpdated = prev.PeakUpdated
		next.PeakUpdatedReady = prev.PeakUpdatedReady
		next.PeakAvailable = prev.PeakAvailable
	}
	if next.Updated > next.PeakUpdated {
		next.PeakUpdated = next.Updated
	}
	if next.UpdatedReady > next.PeakUpdatedReady {
		next.PeakUpdatedReady = next.UpdatedReady
	}
	if next.Available > next.PeakAvailable {
		next.PeakAvailable = next.Available
	}
}
