package types

import (
	"fmt"
	"path"
	"strconv"
	"time"
)

// RunIfType selects when an InitStep should execute. See
// _docs/designs/RUNE-121-Service-Init-Steps.md §3 for the rationale.
type RunIfType string

const (
	// RunIfFreshVolume is the default: the step runs only when the
	// parent volume(s) have not been successfully initialised for the
	// owning service before. Anchored on
	// Volume.Status.InitializedFor[serviceID].
	RunIfFreshVolume RunIfType = "freshVolume"

	// RunIfFileMissing runs the step only when RunIf.Path does not
	// exist inside the parent volume(s).
	RunIfFileMissing RunIfType = "fileMissing"

	// RunIfAlways runs the step on every instance start.
	RunIfAlways RunIfType = "always"
)

// InitStepRestartPolicy controls per-step retry behaviour. The
// instance-level RestartPolicy does not apply to init steps because
// init failure must be observable rather than silently looped on.
type InitStepRestartPolicy string

const (
	// InitStepRestartOnFailure retries the step a small bounded number
	// of times with backoff before failing the instance. Default.
	InitStepRestartOnFailure InitStepRestartPolicy = "OnFailure"

	// InitStepRestartNever fails the instance on any non-zero exit.
	InitStepRestartNever InitStepRestartPolicy = "Never"
)

// RunIf encodes the predicate that decides whether an InitStep runs on
// a given instance start. Validated by Service.Validate.
type RunIf struct {
	// Type selects the predicate. Empty is treated as RunIfFreshVolume.
	Type RunIfType `json:"type,omitempty" yaml:"type,omitempty"`

	// Path is the absolute in-container path tested when
	// Type=fileMissing. Required iff Type=fileMissing.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// Volume optionally restricts a fileMissing check to one named
	// parent volume. When empty, any mounted parent volume that
	// contains the path counts.
	Volume string `json:"volume,omitempty" yaml:"volume,omitempty"`
}

// InitStep is a one-shot container or process executed before the
// main container of a Service starts. See RUNE-121.
type InitStep struct {
	// Name is the step's DNS-1123 identifier, unique within the
	// owning service's InitSteps.
	Name string `json:"name" yaml:"name"`

	// Image is the container image to run. Required when the parent
	// service Runtime is "container" (or unset). For Runtime "process"
	// the field is left empty and the step runs as a subprocess of
	// runed under the parent's process context.
	Image string `json:"image,omitempty" yaml:"image,omitempty"`

	// Command is the executable to run. There is no entrypoint
	// inheritance from the parent image — most init use cases want a
	// different command than the main container.
	Command string `json:"command" yaml:"command"`

	// Args are positional arguments passed to Command.
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`

	// Env is merged on top of the parent service's env. Step-local
	// keys win on conflict.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// EnvFrom is merged with the parent's EnvFrom. Same precedence
	// rules apply.
	EnvFrom []EnvFromSource `json:"envFrom,omitempty" yaml:"envFrom,omitempty"`

	// Volumes is a filter over the parent service's volume mounts.
	// The default (a nil slice) inherits all parent volumes. An empty
	// non-nil slice mounts none. Each entry must reference a parent
	// VolumeMount.Name.
	Volumes []string `json:"volumes,omitempty" yaml:"volumes,omitempty"`

	// SecretMounts behaves identically to Volumes but for the
	// parent's SecretMounts.
	SecretMounts []string `json:"secretMounts,omitempty" yaml:"secretMounts,omitempty"`

	// ConfigmapMounts behaves identically to Volumes but for the
	// parent's ConfigmapMounts.
	ConfigmapMounts []string `json:"configmapMounts,omitempty" yaml:"configmapMounts,omitempty"`

	// Resources optionally overrides the inherited parent Resources.
	// Init for databases is often more I/O- and CPU-heavy than steady
	// state, so artificial low defaults would OOM-kill init.
	Resources *Resources `json:"resources,omitempty" yaml:"resources,omitempty"`

	// RunIf decides whether to execute the step on a given instance
	// start. Zero value is treated as Type=freshVolume.
	RunIf RunIf `json:"runIf,omitempty" yaml:"runIf,omitempty"`

	// Timeout is the per-step ceiling. Zero defers to the cast-level
	// timeout enforced by the controller.
	Timeout time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// RestartPolicy controls per-step retry behaviour. Default
	// OnFailure (bounded retries inside the step).
	RestartPolicy InitStepRestartPolicy `json:"restartPolicy,omitempty" yaml:"restartPolicy,omitempty"`

	// SecurityContext optionally overrides container security knobs for
	// this step (seccomp profile, Linux capabilities, privileged). When
	// nil the step inherits the runtime defaults. Setting privileged=true
	// or seccompProfile.type=unconfined requires the services.privileged
	// policy verb; the server rejects requests that lack it.
	SecurityContext *SecurityContext `json:"securityContext,omitempty" yaml:"securityContext,omitempty"`
}

// SecurityContext bundles container-level security knobs surfaced to
// users. Mirrors generated.SecurityContext on the wire and is also
// allowed on Service for the main container.
type SecurityContext struct {
	// SeccompProfile selects the seccomp policy applied to the
	// container. Nil means the runtime default profile.
	SeccompProfile *SeccompProfile `json:"seccompProfile,omitempty" yaml:"seccompProfile,omitempty"`

	// CapAdd are Linux capabilities granted to the container in
	// addition to the runtime defaults (e.g. "SYS_ADMIN", "NET_ADMIN").
	CapAdd []string `json:"capAdd,omitempty" yaml:"capAdd,omitempty"`

	// CapDrop are Linux capabilities removed from the container,
	// applied after CapAdd.
	CapDrop []string `json:"capDrop,omitempty" yaml:"capDrop,omitempty"`

	// Privileged grants full access to host devices and namespaces.
	// Admin-gated by the services.privileged policy verb.
	Privileged bool `json:"privileged,omitempty" yaml:"privileged,omitempty"`
}

// SeccompProfileType enumerates the supported seccomp profile selectors.
type SeccompProfileType string

const (
	// SeccompProfileDefault uses the runtime's default profile.
	SeccompProfileDefault SeccompProfileType = "default"

	// SeccompProfileUnconfined disables seccomp filtering. Admin-gated.
	SeccompProfileUnconfined SeccompProfileType = "unconfined"

	// SeccompProfileLocalhost loads a profile from a path on the host.
	SeccompProfileLocalhost SeccompProfileType = "localhost"
)

// SeccompProfile selects a seccomp policy.
type SeccompProfile struct {
	// Type selects the profile family. Empty is treated as "default".
	Type SeccompProfileType `json:"type,omitempty" yaml:"type,omitempty"`

	// LocalhostProfile is an absolute path to a JSON seccomp profile
	// on the runtime host. Required iff Type=localhost.
	LocalhostProfile string `json:"localhostProfile,omitempty" yaml:"localhostProfile,omitempty"`
}

// RequiresPrivilegedGate reports whether this SecurityContext contains
// fields that the server should reject for callers without the
// services.privileged policy verb.
func (sc *SecurityContext) RequiresPrivilegedGate() bool {
	if sc == nil {
		return false
	}
	if sc.Privileged {
		return true
	}
	if sc.SeccompProfile != nil && sc.SeccompProfile.Type == SeccompProfileUnconfined {
		return true
	}
	return false
}

// Validate checks structural invariants on SecurityContext. It does
// not enforce policy gates (those are checked server-side via RBAC).
func (sc *SecurityContext) Validate() error {
	if sc == nil {
		return nil
	}
	if sp := sc.SeccompProfile; sp != nil {
		switch sp.Type {
		case "", SeccompProfileDefault, SeccompProfileUnconfined:
			if sp.LocalhostProfile != "" {
				return NewValidationError("securityContext.seccompProfile.localhostProfile only valid with type=localhost")
			}
		case SeccompProfileLocalhost:
			if sp.LocalhostProfile == "" {
				return NewValidationError("securityContext.seccompProfile.localhostProfile is required when type=localhost")
			}
			if !path.IsAbs(sp.LocalhostProfile) {
				return NewValidationError("securityContext.seccompProfile.localhostProfile must be an absolute path")
			}
		default:
			return NewValidationError("securityContext.seccompProfile.type must be one of: default, unconfined, localhost")
		}
	}
	return nil
}

// InitStepStatus is the lifecycle state of one InitStep on one Instance.
type InitStepStatus string

const (
	InitStepStatusPending   InitStepStatus = "Pending"
	InitStepStatusRunning   InitStepStatus = "Running"
	InitStepStatusSucceeded InitStepStatus = "Succeeded"
	InitStepStatusFailed    InitStepStatus = "Failed"
	InitStepStatusSkipped   InitStepStatus = "Skipped"
)

// InitStepReason is a short, machine-friendly slug rolled up by the
// instance controller when an InitStep terminates non-Succeeded.
const (
	InitStepReasonImageUnreachable = "ImageUnreachable"
	InitStepReasonNonZeroExit      = "NonZeroExit"
	InitStepReasonTimeout          = "Timeout"
	InitStepReasonRuntimeError     = "RuntimeError"
)

// InitStepState records the per-instance, per-step execution outcome
// persisted on Instance.InitStates.
type InitStepState struct {
	// Name matches Service.InitSteps[*].Name.
	Name string `json:"name" yaml:"name"`

	// Status is the lifecycle state.
	Status InitStepStatus `json:"status" yaml:"status"`

	// ExitCode is the process/container exit code. Zero by default;
	// only meaningful when Status is Succeeded or Failed.
	ExitCode int `json:"exitCode,omitempty" yaml:"exitCode,omitempty"`

	// StartedAt is the wall-clock time the step entered Running.
	StartedAt time.Time `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`

	// FinishedAt is the wall-clock time the step left Running.
	FinishedAt time.Time `json:"finishedAt,omitempty" yaml:"finishedAt,omitempty"`

	// Attempts is the number of executions tried so far (>= 1 once
	// the step has run at least once). Used by RestartPolicy=OnFailure.
	Attempts int `json:"attempts,omitempty" yaml:"attempts,omitempty"`

	// Reason is one of the InitStepReason* slugs (empty on success).
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`

	// Message is a single-line human explanation copied to
	// Service.StatusMessage on rollup.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	// LogRef is an opaque handle the log subsystem can resolve to the
	// step's stdout/stderr slice.
	LogRef string `json:"logRef,omitempty" yaml:"logRef,omitempty"`
}

// ServiceReasonInitStepFailed is the Service.StatusReason value
// surfaced when any init step on any instance terminates Failed.
const ServiceReasonInitStepFailed = "InitStepFailed"

// InstanceStatusInitializing is the new instance phase between
// Pending (volumes bound) and Starting (main container Created/Start
// dispatched).
const InstanceStatusInitializing InstanceStatus = "Initializing"

// validateInitSteps returns the first validation error encountered in
// the Service's InitSteps, or nil. Called from Service.Validate.
func (s *Service) validateInitSteps() error {
	if len(s.InitSteps) == 0 {
		return nil
	}

	parentVolumes := make(map[string]struct{}, len(s.Volumes))
	for _, v := range s.Volumes {
		parentVolumes[v.Name] = struct{}{}
	}

	parentSecrets := make(map[string]struct{}, len(s.SecretMounts))
	for _, m := range s.SecretMounts {
		parentSecrets[m.Name] = struct{}{}
	}

	parentConfigmaps := make(map[string]struct{}, len(s.ConfigmapMounts))
	for _, m := range s.ConfigmapMounts {
		parentConfigmaps[m.Name] = struct{}{}
	}

	seenName := make(map[string]struct{}, len(s.InitSteps))
	for i, step := range s.InitSteps {
		ctx := fmt.Sprintf("initSteps[%d]", i)

		if step.Name == "" {
			return NewValidationError(ctx + ": name is required")
		}
		if !isDNS1123Label(step.Name) {
			return NewValidationError(ctx + ": name " + strconv.Quote(step.Name) + " is not a valid DNS-1123 label")
		}
		if _, dup := seenName[step.Name]; dup {
			return NewValidationError(ctx + ": duplicate step name " + strconv.Quote(step.Name))
		}
		seenName[step.Name] = struct{}{}

		switch s.Runtime {
		case "", "container":
			if step.Image == "" {
				return NewValidationError(ctx + " (" + step.Name + "): image is required for container runtime")
			}
		case "process":
			if step.Image != "" {
				return NewValidationError(ctx + " (" + step.Name + "): image must be empty for process runtime")
			}
		}

		if step.Command == "" {
			return NewValidationError(ctx + " (" + step.Name + "): command is required")
		}

		// Volume / secret / configmap filter references must exist on
		// the parent. A nil slice is "inherit all" and is fine; an
		// empty non-nil slice is "mount none" and is also fine.
		for _, name := range step.Volumes {
			if _, ok := parentVolumes[name]; !ok {
				return NewValidationError(ctx + " (" + step.Name + "): volume " + strconv.Quote(name) + " does not match any parent service volume")
			}
		}
		for _, name := range step.SecretMounts {
			if _, ok := parentSecrets[name]; !ok {
				return NewValidationError(ctx + " (" + step.Name + "): secretMount " + strconv.Quote(name) + " does not match any parent service secretMount")
			}
		}
		for _, name := range step.ConfigmapMounts {
			if _, ok := parentConfigmaps[name]; !ok {
				return NewValidationError(ctx + " (" + step.Name + "): configmapMount " + strconv.Quote(name) + " does not match any parent service configmapMount")
			}
		}

		if err := validateInitRunIf(ctx, step, parentVolumes); err != nil {
			return err
		}

		switch step.RestartPolicy {
		case "", InitStepRestartOnFailure, InitStepRestartNever:
		default:
			return NewValidationError(ctx + " (" + step.Name + "): unknown restartPolicy " + strconv.Quote(string(step.RestartPolicy)))
		}

		if step.Timeout < 0 {
			return NewValidationError(ctx + " (" + step.Name + "): timeout cannot be negative")
		}

		if err := step.SecurityContext.Validate(); err != nil {
			return NewValidationError(ctx + " (" + step.Name + "): " + err.Error())
		}
	}

	return nil
}

// validateInitRunIf validates the RunIf clause for one step.
func validateInitRunIf(ctx string, step InitStep, parentVolumes map[string]struct{}) error {
	t := step.RunIf.Type
	if t == "" {
		t = RunIfFreshVolume
	}

	switch t {
	case RunIfFreshVolume:
		// freshVolume must anchor on at least one parent volume.
		// An explicit empty filter (Volumes=[]string{}) means "mount
		// none", which leaves nothing to anchor on.
		hasInherit := step.Volumes == nil && len(parentVolumes) > 0
		hasFilter := len(step.Volumes) > 0
		if !hasInherit && !hasFilter {
			return NewValidationError(ctx + " (" + step.Name + "): runIf.type=freshVolume requires the step to mount at least one parent volume")
		}
		if step.RunIf.Path != "" || step.RunIf.Volume != "" {
			return NewValidationError(ctx + " (" + step.Name + "): runIf.path/volume are only valid with runIf.type=fileMissing")
		}

	case RunIfFileMissing:
		if step.RunIf.Path == "" {
			return NewValidationError(ctx + " (" + step.Name + "): runIf.path is required when runIf.type=fileMissing")
		}
		if !path.IsAbs(step.RunIf.Path) {
			return NewValidationError(ctx + " (" + step.Name + "): runIf.path must be absolute")
		}
		// The step must actually mount a parent volume so the path
		// can be checked.
		hasInherit := step.Volumes == nil && len(parentVolumes) > 0
		hasFilter := len(step.Volumes) > 0
		if !hasInherit && !hasFilter {
			return NewValidationError(ctx + " (" + step.Name + "): runIf.type=fileMissing requires the step to mount at least one parent volume")
		}
		if step.RunIf.Volume != "" {
			if _, ok := parentVolumes[step.RunIf.Volume]; !ok {
				return NewValidationError(ctx + " (" + step.Name + "): runIf.volume " + strconv.Quote(step.RunIf.Volume) + " does not match any parent service volume")
			}
			// If the step has an explicit filter, the named volume
			// must be in it.
			if hasFilter {
				inFilter := false
				for _, n := range step.Volumes {
					if n == step.RunIf.Volume {
						inFilter = true
						break
					}
				}
				if !inFilter {
					return NewValidationError(ctx + " (" + step.Name + "): runIf.volume " + strconv.Quote(step.RunIf.Volume) + " is not in the step's volumes filter")
				}
			}
		}

	case RunIfAlways:
		if step.RunIf.Path != "" || step.RunIf.Volume != "" {
			return NewValidationError(ctx + " (" + step.Name + "): runIf.path/volume are only valid with runIf.type=fileMissing")
		}

	default:
		return NewValidationError(ctx + " (" + step.Name + "): unknown runIf.type " + strconv.Quote(string(t)))
	}

	return nil
}

// isDNS1123Label is a minimal DNS-1123 label check (lowercase a–z, 0–9,
// '-', length 1–63, no leading/trailing '-').
func isDNS1123Label(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	// strconv is imported for the Quote helper used above.
	return true
}
