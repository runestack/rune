package types

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// oomTokenRe matches "oom" only as a standalone token (surrounded by
// non-letters), so it recognises real OOM signals ("OOM", "oom killed")
// without firing on innocent substrings — most importantly a service NAMED
// with "oom" in it, e.g. "greenroom", which previously misreported as
// OutOfMemory. "OOMKilled"/"out of memory" are matched explicitly.
var oomTokenRe = regexp.MustCompile(`(^|[^a-z])oom([^a-z]|$)`)

var _ Resource = (*Service)(nil)

// ImagePull values for Service.ImagePull.
const (
	ImagePullAlways  = "always"
	ImagePullMissing = "missing"
	ImagePullNever   = "never"
)

// Service represents a deployable application or workload.
type Service struct {
	NamespacedResource `json:"-" yaml:"-"`

	// Unique identifier for the service
	ID string `json:"id" yaml:"id"`

	// Human-readable name for the service // DNS-1123 unique name within a namespace
	Name string `json:"name" yaml:"name"`

	// Namespace the service belongs to
	Namespace string `json:"namespace" yaml:"namespace"`

	// Labels are key/value pairs that can be used to organize and categorize services
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// Container image for the service
	Image string `json:"image,omitempty" yaml:"image,omitempty"`

	// Named registry selector for pulling the image (optional)
	ImageRegistry string `json:"imageRegistry,omitempty" yaml:"imageRegistry,omitempty"`

	// Registry override allowing inline auth or named selection (optional)
	Registry *ServiceRegistryOverride `json:"registry,omitempty" yaml:"registry,omitempty"`

	// ImagePull controls when the runner pulls the container image.
	// Allowed values: "always", "missing", "never". Empty defaults to
	// "always" (re-pull on every deploy/restart).
	ImagePull string `json:"imagePull,omitempty" yaml:"imagePull,omitempty"`

	// ImagePullAnonymous forces this image to be pulled WITHOUT registry
	// credentials, even when a configured registry entry matches its host.
	// Use it for a public image on a registry you also hold private
	// credentials for — otherwise a host-wide credential is attached to the
	// pull, and an expired token breaks an image that needs no auth.
	ImagePullAnonymous bool `json:"imagePullAnonymous,omitempty" yaml:"imagePullAnonymous,omitempty"`

	// Command to run in the container (overrides image CMD)
	Command string `json:"command,omitempty" yaml:"command,omitempty"`

	// Arguments to the command
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`

	// Environment variables for the service
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// Imported environment variables sources (normalized from spec)
	EnvFrom []EnvFromSource `json:"envFrom,omitempty" yaml:"envFrom,omitempty"`

	// Number of instances to run
	Scale int `json:"scale" yaml:"scale"`

	// Ports exposed by the service
	Ports []ServicePort `json:"ports,omitempty" yaml:"ports,omitempty"`

	// Resource requirements for each instance
	Resources Resources `json:"resources,omitempty" yaml:"resources,omitempty"`

	// Health checks for the service
	Health *HealthCheck `json:"health,omitempty" yaml:"health,omitempty"`

	// Network policy for controlling traffic
	NetworkPolicy *ServiceNetworkPolicy `json:"networkPolicy,omitempty" yaml:"networkPolicy,omitempty"`

	// External exposure configuration
	Expose *ServiceExpose `json:"expose,omitempty" yaml:"expose,omitempty"`

	// Placement preferences and requirements
	Affinity *ServiceAffinity `json:"affinity,omitempty" yaml:"affinity,omitempty"`

	// Autoscaling configuration
	Autoscale *ServiceAutoscale `json:"autoscale,omitempty" yaml:"autoscale,omitempty"`

	// Secret mounts
	SecretMounts []SecretMount `json:"secretMounts,omitempty" yaml:"secretMounts,omitempty"`

	// Config mounts
	ConfigmapMounts []ConfigmapMount `json:"configmapMounts,omitempty" yaml:"configmapMounts,omitempty"`

	// Volume mounts. Each entry references either a pre-existing
	// VolumeClaim by name (Claim) or an inline ClaimTemplate that the
	// VolumeController materializes into a per-instance Volume.
	Volumes []VolumeMount `json:"volumes,omitempty" yaml:"volumes,omitempty"`

	// InitSteps run sequentially before each instance's main
	// container starts. They share the parent's volumes / secret /
	// config / env by default. See RUNE-121.
	InitSteps []InitStep `json:"initSteps,omitempty" yaml:"initSteps,omitempty"`

	// SecurityContext applied to the main container. Privileged=true
	// and seccompProfile.type=unconfined are gated server-side behind
	// the services.privileged policy verb.
	SecurityContext *SecurityContext `json:"securityContext,omitempty" yaml:"securityContext,omitempty"`

	// Service discovery configuration
	Discovery *ServiceDiscovery `json:"discovery,omitempty" yaml:"discovery,omitempty"`

	// Dependencies this service declares (normalized internal form)
	Dependencies []DependencyRef `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`

	// Status of the service
	Status ServiceStatus `json:"status" yaml:"status"`

	// StatusReason is a short, machine-friendly slug describing why
	// the service is in its current Status. Set by the reconciler when
	// rolling up the worst instance state. Examples: "ImageUnreachable",
	// "Unhealthy", "OutOfMemory", "Unplaceable", "ConfigMissing".
	// Empty when Status is Running.
	StatusReason string `json:"statusReason,omitempty" yaml:"statusReason,omitempty"`

	// StatusMessage is a single human-readable sentence explaining
	// StatusReason, copied verbatim from the worst instance's
	// StatusMessage. The CLI shows this directly on `rune get
	// service <name>` and on cast failure so developers never have to
	// run a second command to learn why a service is unhealthy.
	StatusMessage string `json:"statusMessage,omitempty" yaml:"statusMessage,omitempty"`

	// IngressCert tracks asynchronous TLS certificate state for the
	// ingress controller (RUNE-066). Populated only when Expose is
	// configured for ACME-managed TLS. The orchestrator updates this
	// field independently of cast; cast does not block on issuance.
	IngressCert *IngressCertStatus `json:"ingressCert,omitempty" yaml:"ingressCert,omitempty"`

	// Instances of this service currently running
	Instances []Instance `json:"instances,omitempty" yaml:"instances,omitempty"`

	// Runtime for the service ("container" or "process")
	Runtime RuntimeType `json:"runtime,omitempty" yaml:"runtime,omitempty"`

	// Process-specific configuration (when Runtime="process")
	Process *ProcessSpec `json:"process,omitempty" yaml:"process,omitempty"`

	// Restart policy for the service
	RestartPolicy RestartPolicy `json:"restart_policy,omitempty" yaml:"restart_policy,omitempty"`

	// UpdateStrategy is how instances are replaced when the template changes
	// (RUNE-042). nil means rolling. See pkg/types/update_strategy.go.
	UpdateStrategy *UpdateStrategy `json:"updateStrategy,omitempty" yaml:"updateStrategy,omitempty"`

	// DrainSeconds is the shutdown grace for this service's instances: how
	// long an instance keeps serving in-flight work after being withdrawn
	// from the dataplane endpoint set, before SIGTERM. Governs EVERY
	// teardown — scale-down, stop, deletion, liveness restart — not just
	// updates, which is why it is a service-level field rather than a member
	// of UpdateStrategy. nil means DefaultDrainSeconds; floored at
	// MinDrainSeconds. Read it via Service.DrainWindow().
	DrainSeconds *int `json:"drainSeconds,omitempty" yaml:"drainSeconds,omitempty"`

	// Update is the progress of an in-flight update; nil when none is
	// running. Reconciler-owned observed state, written inside the same CAS
	// status write as Status/StatusReason (RUNE-042 §8.2).
	Update *UpdateStatus `json:"update,omitempty" yaml:"update,omitempty"`

	// Metadata for the service
	Metadata *ServiceMetadata `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// EnvFromSource is the internal normalized representation of envFrom
type EnvFromSource struct {
	// Exactly one of these will be set
	SecretName    string `json:"secretName,omitempty" yaml:"secretName,omitempty"`
	ConfigmapName string `json:"configmapName,omitempty" yaml:"configmapName,omitempty"`

	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Prefix    string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
}

func (s *Service) GetResourceType() ResourceType {
	return ResourceTypeService
}

// ServiceMetadata represents metadata for a service.
type ServiceMetadata struct {
	// Creation timestamp
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`

	// Generation of the service
	Generation int64 `json:"generation,omitempty" yaml:"generation,omitempty"`

	// TemplateGeneration tracks changes to the instance TEMPLATE — the parts
	// of the spec that define what a container looks like (image, env, mounts,
	// ports, ...) — as opposed to Generation, which advances on EVERY
	// desired-state change including scale. Cast bumps both (to the same
	// value); the scaling controller bumps only Generation. Instances record
	// TemplateGeneration at creation, and the compatibility check compares
	// against it — so a scale-up never recreates surviving containers, while
	// a spec change still does (issue #142; mirrors the K8s
	// Deployment-generation vs pod-template-hash split). Pre-existing services
	// have 0 here, which compares as "not newer than any instance" until the
	// next cast stamps it.
	TemplateGeneration int64 `json:"templateGeneration,omitempty" yaml:"templateGeneration,omitempty"`

	// ObservedGeneration is the Generation the reconciler last acted on. When it
	// equals Generation, the desired state (spec + scale) is fully reconciled, so
	// an incoming update carries only status/observed fields and reconciliation
	// can be skipped (see serviceController.isStatusOnlyChange). Every
	// desired-state change bumps Generation — including scale, which the scaling
	// controller now increments — so this single persisted field replaces the
	// in-memory serviceObservedGenerations/serviceObservedScales shadow maps and,
	// unlike them, survives a runed restart (RFC #129 Phase 2).
	ObservedGeneration int64 `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`

	// OwnedBy is the system ownership stamp set when a runeset release manages
	// this service. See _docs/plugins/RUNESET_STATEFUL_RELEASES.md.
	OwnedBy *OwnedBy `json:"ownedBy,omitempty" yaml:"ownedBy,omitempty"`

	// LastNonZeroScale tracks the most recent non-zero scale to support restart semantics
	LastNonZeroScale int `json:"lastNonZeroScale,omitempty" yaml:"lastNonZeroScale,omitempty"`

	// DeletionTimestamp marks the service as being torn down (Kubernetes-style
	// foreground deletion, RFC #129 Phase 4). Once set, the reconciler drives
	// reconcileDeletion instead of reconciling toward the spec, and the record
	// is NOT removed from the store until every finalizer has cleared. Being
	// persisted, an interrupted teardown resumes on the next reconcile with no
	// separate recovery path.
	DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty" yaml:"deletionTimestamp,omitempty"`

	// Finalizers are the ordered cleanup steps that must complete before the
	// service record may be removed. Populated when DeletionTimestamp is set
	// (instance-cleanup, then volume-cleanup iff the service has claimTemplate
	// volumes). The reconciler pops each entry only after its work is fully
	// done; when the list is empty the record is deleted (the terminal
	// transition). This gate is what makes the record provably outlive its
	// instances and volumes — orphans become impossible by construction.
	Finalizers []FinalizerType `json:"finalizers,omitempty" yaml:"finalizers,omitempty"`
}

// ServicePort represents a port exposed by a service.
type ServicePort struct {
	// Name for this port (used in references)
	Name string `json:"name" yaml:"name"`

	// Port number
	Port int `json:"port" yaml:"port"`

	// Target port (if different from port)
	TargetPort int `json:"targetPort,omitempty" yaml:"targetPort,omitempty"`

	// Protocol (default: TCP)
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`

	// HostPort, when > 0, publishes the container's port to the host on
	// 127.0.0.1:<HostPort>. Intended as a dev-mode escape hatch on
	// platforms where the cluster dataplane cannot reach the container
	// bridge IP from the host (notably macOS Docker Desktop). Production
	// services should reach services through the cluster VIP or ingress,
	// not via a published host port.
	HostPort int `json:"hostPort,omitempty" yaml:"hostPort,omitempty"`
}

// ServiceExpose defines how a service is exposed externally.
type ServiceExpose struct {
	// Port or port name to expose
	Port string `json:"port" yaml:"port"`

	// Host for the exposed service
	Host string `json:"host,omitempty" yaml:"host,omitempty"`

	// Path prefix for the exposed service
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// TLS configuration for the exposed service
	TLS *ExposeServiceTLS `json:"tls,omitempty" yaml:"tls,omitempty"`

	// AllowCIDRs restricts inbound connections to these source CIDRs,
	// enforced at the ingress listener against the real TCP peer (not a
	// forwarding header). Empty means "no restriction" (allow all) — never
	// deny-all. Defense-in-depth for origin lockdown behind a CDN. Only
	// meaningful when ingress is the direct TCP terminator (no L4 LB in
	// front rewriting the source IP).
	AllowCIDRs []string `json:"allowCidrs,omitempty" yaml:"allowCidrs,omitempty"`

	// ClientCert requires a trusted client certificate (mTLS) on inbound
	// TLS connections, verified at the ingress handshake. The primary
	// origin-lockdown control — strongest when the CA is account-specific
	// (not a CDN's shared origin-pull CA).
	ClientCert *ExposeClientCert `json:"clientCert,omitempty" yaml:"clientCert,omitempty"`
}

// ExposeClientCert configures inbound mTLS for an exposed service.
type ExposeClientCert struct {
	// CASecret references a Secret holding a PEM CA bundle (data key
	// "ca.crt") used to verify the client certificate. Accepts the same
	// resource-ref shapes as ExposeServiceTLS.Secret.
	CASecret string `json:"caSecret" yaml:"caSecret"`

	// Mode is the verification strictness. v1 supports only "require"
	// (RequireAndVerifyClientCert); empty defaults to "require".
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
}

// Well-known ExposeClientCert.Mode values.
const (
	ClientCertModeRequire = "require"
)

// ExposeServiceTLS defines TLS configuration for exposed services.
type ExposeServiceTLS struct {
	// Secret references a Secret resource holding tls.crt + tls.key
	// for operator-supplied certificates. Accepts three shapes:
	//
	//   - "name"                       (resolved in the service's namespace)
	//   - "<namespace>/<name>"         (cross-namespace shorthand)
	//   - "secret:<name>.<ns>.rune"    (FQDN secret ref)
	//
	// Required when Mode is "manual".
	Secret string `json:"secret,omitempty" yaml:"secret,omitempty"`

	// Whether to automatically generate a TLS certificate
	Auto bool `json:"auto,omitempty" yaml:"auto,omitempty"`

	// Mode selects the certificate provisioning strategy.
	// Empty / "manual": operator supplies Secret.
	// "acme": ingress controller obtains a cert from Let's Encrypt
	// (HTTP-01) for Expose.Host. Auto implies Mode=acme.
	// See RUNE-066.
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
}

// Well-known TLS modes for ExposeServiceTLS.Mode.
const (
	ExposeTLSModeManual = "manual"
	ExposeTLSModeACME   = "acme"
	// ExposeTLSModeAuto is a user-friendly synonym for ExposeTLSModeACME.
	// Both "auto" and "acme" request a Let's Encrypt-issued certificate.
	ExposeTLSModeAuto = "auto"
)

// IsACME reports whether the expose configuration requests
// ACME-managed TLS. The boolean Auto: true is treated as Mode=acme.
// Mode values "acme" and "auto" are accepted as synonyms.
func (t *ExposeServiceTLS) IsACME() bool {
	if t == nil {
		return false
	}
	if t.Mode == ExposeTLSModeACME || t.Mode == ExposeTLSModeAuto {
		return true
	}
	return t.Auto && t.Mode == ""
}

// ServiceDiscoverySpec is the operator-facing discovery block in cast
// YAML. It intentionally has no VIP field — the control plane assigns
// a stable cluster VIP at service create time (RUNE-040).
type ServiceDiscoverySpec struct {
	Mode               string `json:"mode,omitempty" yaml:"mode,omitempty"`
	LocalityPreference string `json:"localityPreference,omitempty" yaml:"localityPreference,omitempty"`
}

// ServiceDiscovery is the persisted service discovery state (API/store).
type ServiceDiscovery struct {
	// Discovery mode (load-balanced or headless)
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`

	// VIP is the cluster virtual IP allocated for this service. Set by
	// the control plane on Create via the cluster VIP allocator
	// (RUNE-040). Stable for the lifetime of the service ID.
	VIP string `json:"vip,omitempty" yaml:"vip,omitempty"`

	// LocalityPreference controls how the userspace proxy picks
	// endpoints (RUNE-041). One of "" (no preference), "prefer-local"
	// (same-node first, fall back to remote), or "local-only" (fail
	// closed if no local endpoint).
	LocalityPreference string `json:"localityPreference,omitempty" yaml:"localityPreference,omitempty"`
}

// DependencyRef is the normalized internal representation of a dependency
// It can represent Service, Secret, or Configmap dependencies.
// Exactly one of Service/Secret/Configmap should be set.
type DependencyRef struct {
	Service   string `json:"service,omitempty" yaml:"service,omitempty"`
	Secret    string `json:"secret,omitempty" yaml:"secret,omitempty"`
	Configmap string `json:"configmap,omitempty" yaml:"configmap,omitempty"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

func (d *DependencyRef) GetDependencyResourceType() ResourceType {
	if d.Secret != "" {
		return ResourceTypeSecret
	}
	if d.Configmap != "" {
		return ResourceTypeConfigmap
	}
	return ResourceTypeService
}

func (d *DependencyRef) GetDependencyResourceName() string {
	if d.Secret != "" {
		return d.Secret
	}
	if d.Configmap != "" {
		return d.Configmap
	}
	return d.Service
}

// ServiceAffinity defines placement rules for a service.
type ServiceAffinity struct {
	// Hard constraints (service can only run on nodes matching these)
	Required []string `json:"required,omitempty" yaml:"required,omitempty"`

	// Soft preferences (scheduler will try to place on nodes matching these)
	Preferred []string `json:"preferred,omitempty" yaml:"preferred,omitempty"`

	// Run instances near services matching these labels
	With []string `json:"with,omitempty" yaml:"with,omitempty"`

	// Avoid running instances on nodes with services matching these labels
	Avoid []string `json:"avoid,omitempty" yaml:"avoid,omitempty"`

	// Try to distribute instances across this topology key (e.g., "zone")
	Spread string `json:"spread,omitempty" yaml:"spread,omitempty"`
}

// ServiceAutoscale defines autoscaling behavior for a service.
type ServiceAutoscale struct {
	// Whether autoscaling is enabled
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Minimum number of instances
	Min int `json:"min" yaml:"min"`

	// Maximum number of instances
	Max int `json:"max" yaml:"max"`

	// Metric to scale on (e.g., cpu, memory)
	Metric string `json:"metric" yaml:"metric"`

	// Target value for the metric (e.g., 70%)
	Target string `json:"target" yaml:"target"`

	// Cooldown period between scaling events
	Cooldown string `json:"cooldown,omitempty" yaml:"cooldown,omitempty"`

	// Maximum number of instances to add/remove in a single scaling event
	Step int `json:"step,omitempty" yaml:"step,omitempty"`
}

// ServiceStatus represents the current status of a service.
type ServiceStatus string

const (
	// ServiceStatusPending indicates the service is being created.
	ServiceStatusPending ServiceStatus = "Pending"

	// ServiceStatusRunning indicates the service is running.
	ServiceStatusRunning ServiceStatus = "Running"

	// ServiceStatusDeploying indicates the service is being updated.
	ServiceStatusDeploying ServiceStatus = "Deploying"

	// ServiceStatusStopping indicates the service has a lower desired
	// scale than its current instance count — instances are actively being
	// torn down to reach the desired state. Set during `rune stop` and the
	// drain phase of `rune restart`. Visible distinct from "Running" so
	// operators can tell at a glance that a service is in flight, not idle.
	ServiceStatusStopping ServiceStatus = "Stopping"

	// ServiceStatusFailed indicates the service failed to deploy or run.
	ServiceStatusFailed ServiceStatus = "Failed"

	// ServiceStatusDeleted indicates the service has been deleted.
	ServiceStatusDeleted ServiceStatus = "Deleted"
)

// Well-known StatusReason values for Service.StatusReason. The reconciler
// derives one of these from the worst-instance state. Keep this set small
// and stable — operators may script against it.
//
// These names are intentionally Rune-shaped (verbs and plain English),
// not borrowed from Kubernetes' Pod conditions.
const (
	ServiceReasonImageUnreachable = "ImageUnreachable" // image can't be pulled (auth, missing tag, network)
	ServiceReasonUnhealthy        = "Unhealthy"        // liveness/readiness probe failing
	ServiceReasonRestarting       = "Restarting"       // instance keeps exiting and being restarted
	ServiceReasonOutOfMemory      = "OutOfMemory"      // killed for exceeding the memory limit
	ServiceReasonUnplaceable      = "Unplaceable"      // no node has the capacity / matches placement
	ServiceReasonConfigMissing    = "ConfigMissing"    // referenced secret/configmap/env not found
	ServiceReasonLaunchFailed     = "LaunchFailed"     // runner refused to start the instance
	ServiceReasonExited           = "Exited"           // instance ran to completion (non-zero or otherwise)
	ServiceReasonUnknown          = "Unknown"          // no recognisable signal

	// RUNE-042. "Update" is the user-facing word throughout — the spec field
	// is updateStrategy, so reasons, events, CLI and docs all say update.
	// "Rollout" and "surge" stay internal vocabulary.
	ServiceReasonUpdating      = "Updating"      // a rolling update is in progress
	ServiceReasonUpdateStalled = "UpdateStalled" // no progress within the stall deadline

	// GPU admission. Rune-shaped like the rest of this block, not
	// borrowed from Kubernetes Pod conditions. NoGpuCapacity rather than
	// plain Unplaceable because the remedy differs enough to be worth
	// scripting against separately: free VRAM, versus fix a selector.
	GPUReasonNoCapacity     = "NoGpuCapacity"     // no device has room for the request
	GPUReasonToolkitMissing = "GpuToolkitMissing" // GPUs present, container runtime not configured
	GPUReasonDeviceMissing  = "GpuDeviceMissing"  // an assigned device is gone
	GPUReasonOverCommitted  = "GpuOverCommitted"  // an assignment no longer fits

	// GPUReasonInventoryUnknown is the one RETRYABLE slug in this family:
	// the node has not reported its devices yet. Callers branch on it, so
	// it belongs in the vocabulary beside the refusals rather than as a
	// literal somewhere.
	GPUReasonInventoryUnknown = "GpuInventoryUnknown"
)

// DeriveServiceReason inspects an instance's status and message and
// returns a short, stable Service.StatusReason slug. Used by the
// reconciler to roll up an unhealthy instance into a service-level
// reason that's friendly to display in tables and to script against.
func DeriveServiceReason(status InstanceStatus, message string) string {
	m := strings.ToLower(message)
	switch {
	case strings.Contains(m, "pull access denied"),
		strings.Contains(m, "manifest unknown"),
		strings.Contains(m, "image not found"),
		strings.Contains(m, "no such image"),
		strings.Contains(m, "imagepullbackoff"),
		strings.Contains(m, "errimagepull"),
		strings.Contains(m, "pull failed"),
		strings.Contains(m, "unauthorized") && strings.Contains(m, "image"):
		return ServiceReasonImageUnreachable
	case strings.Contains(m, "oomkilled"),
		strings.Contains(m, "out of memory"),
		oomTokenRe.MatchString(m):
		return ServiceReasonOutOfMemory
	case strings.Contains(m, "probe"), strings.Contains(m, "health check"):
		return ServiceReasonUnhealthy
	case strings.Contains(m, "no node"), strings.Contains(m, "no capacity"),
		strings.Contains(m, "schedule"):
		return ServiceReasonUnplaceable
	case strings.Contains(m, "secret"), strings.Contains(m, "configmap"),
		strings.Contains(m, "config map"), strings.Contains(m, "env"):
		if strings.Contains(m, "not found") || strings.Contains(m, "unresolvable") {
			return ServiceReasonConfigMissing
		}
	case strings.Contains(m, "crashloop"), strings.Contains(m, "restart"):
		return ServiceReasonRestarting
	}
	switch status {
	case InstanceStatusExited:
		return ServiceReasonExited
	case InstanceStatusFailed:
		return ServiceReasonLaunchFailed
	case InstanceStatusUnknown:
		return ServiceReasonUnknown
	}
	return ServiceReasonLaunchFailed
}

// Resources represents resource requirements for a service instance.
type Resources struct {
	// CPU request in millicores (1000m = 1 CPU)
	CPU ResourceLimit `json:"cpu,omitempty" yaml:"cpu,omitempty"`

	// Memory request in bytes
	Memory ResourceLimit `json:"memory,omitempty" yaml:"memory,omitempty"`

	// GPU is an accelerator request. A pointer because absent and
	// "present but empty" mean different things: `gpu: {}` is a request
	// for one whole device, and a zero-valued struct could not say that.
	GPU *GPURequest `json:"gpu,omitempty" yaml:"gpu,omitempty"`
}

// GPURequest asks for accelerator capacity. Request-only: there is no
// limit field, because outside the engine's own arena Rune cannot enforce
// a VRAM ceiling, and a `limit` that is not enforced is a lie told in the
// spec grammar.
//
// The two shapes:
//
//   - `gpu: {}` or `count: N` — N whole devices, exclusively yours.
//   - `vram: "20Gi"` — a share of one device. Rune sums the vram requests
//     assigned to a device and refuses to exceed what it physically has.
//
// vram covers the WHOLE process footprint on the device — weights, KV
// cache and the CUDA context — not just the weights. Sizing it from a
// parameter count alone under-requests.
//
// There is deliberately no `vendor` field. One value would be legal, so
// it could only ever be typed wrong; the device record carries Vendor and
// the auto-labels give a multi-vendor box a nodeSelector path. The name
// is reserved, not accepted.
type GPURequest struct {
	// Count is how many whole devices to reserve. Zero means one: a
	// bare `gpu: {}` is the common case and must not have to say so.
	Count int `json:"count,omitempty" yaml:"count,omitempty"`

	// VRAM is a per-device memory request in ParseMemory units ("20Gi").
	// Set it to share a device; leave it empty to take whole devices.
	VRAM string `json:"vram,omitempty" yaml:"vram,omitempty"`

	// AllowHeterogeneous permits a multi-device request to span
	// different products. Off by default because tensor parallelism
	// across mismatched cards fails at model load with an error that
	// names neither card.
	AllowHeterogeneous bool `json:"allowHeterogeneous,omitempty" yaml:"allowHeterogeneous,omitempty"`
}

// DeviceCount is the number of whole devices this request implies. A
// request that shares a device by vram occupies one.
func (g *GPURequest) DeviceCount() int {
	if g == nil {
		return 0
	}
	if g.Count <= 0 {
		return 1
	}
	return g.Count
}

// SharesDevice reports whether this is a vram-share rather than a
// whole-device claim.
func (g *GPURequest) SharesDevice() bool {
	return g != nil && g.VRAM != ""
}

// ResourceLimit defines request and limit for a resource.
type ResourceLimit struct {
	// Requested resources (guaranteed)
	Request string `json:"request,omitempty" yaml:"request,omitempty"`

	// Maximum resources (limit)
	Limit string `json:"limit,omitempty" yaml:"limit,omitempty"`
}

// HealthCheck represents health check configuration for a service.
type HealthCheck struct {
	// Liveness probe checks if the instance is running
	Liveness *Probe `json:"liveness,omitempty" yaml:"liveness,omitempty"`

	// Readiness probe checks if the instance is ready to receive traffic
	Readiness *Probe `json:"readiness,omitempty" yaml:"readiness,omitempty"`
}

// Probe represents a health check probe configuration.
type Probe struct {
	// Type of probe (http, tcp, exec)
	Type string `json:"type" yaml:"type"`

	// HTTP path for http probe
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// Host overrides the default probe target. Empty means use the
	// instance's container IP (production) or localhost (process
	// runner / dev fallback). Set this to "127.0.0.1" when targeting
	// a hostPort published by the service on macOS Docker Desktop.
	Host string `json:"host,omitempty" yaml:"host,omitempty"`

	// Port to connect to
	Port int `json:"port" yaml:"port"`

	// Command to execute for exec probe
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`

	// Initial delay seconds before starting checks
	InitialDelaySeconds int `json:"initialDelaySeconds,omitempty" yaml:"initialDelaySeconds,omitempty"`

	// Interval between checks in seconds
	IntervalSeconds int `json:"intervalSeconds,omitempty" yaml:"intervalSeconds,omitempty"`

	// Timeout for the probe in seconds
	TimeoutSeconds int `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`

	// Failure threshold for the probe
	FailureThreshold int `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`

	// Success threshold for the probe
	SuccessThreshold int `json:"successThreshold,omitempty" yaml:"successThreshold,omitempty"`
}

// RestartPolicy defines how instances should be restarted
type RestartPolicy string

const (
	// RestartPolicyAlways means always restart when not explicitly stopped
	RestartPolicyAlways RestartPolicy = "Always"

	// RestartPolicyOnFailure means only restart on failure
	RestartPolicyOnFailure RestartPolicy = "OnFailure"

	// RestartPolicyNever means never restart automatically, only manual restarts are allowed
	RestartPolicyNever RestartPolicy = "Never"
)

// String returns a unique identifier for the service
func (s *Service) String() string {
	return fmt.Sprintf("%s/%s", s.Namespace, s.Name)
}

// Equals checks if two services are functionally equivalent for watch purposes
func (s *Service) Equals(other Resource) bool {
	otherService, ok := other.(*Service)
	if !ok {
		return false
	}

	// Check key fields that would make a service visibly different in the table
	return s.Name == otherService.Name &&
		s.Namespace == otherService.Namespace &&
		s.Status == otherService.Status &&
		s.Scale == otherService.Scale &&
		s.Image == otherService.Image &&
		s.Runtime == otherService.Runtime
}

// Validate validates the service configuration.
func (s *Service) Validate() error {
	if s.ID == "" {
		return NewValidationError("service ID is required")
	}

	if s.Name == "" {
		return NewValidationError("service name is required")
	}

	// Update strategy, now that the runtime is known — the spec-level check
	// cannot see it, and a process service is one of the cases that can
	// deadlock an update.
	if err := s.UpdateStrategy.Validate(); err != nil {
		return err
	}
	if err := s.UpdateStrategy.ValidateForService(s); err != nil {
		return err
	}

	// Check runtime specific requirements
	switch s.Runtime {
	case "container", "":
		// Default is container runtime
		if s.Image == "" {
			return NewValidationError("service image is required for container runtime")
		}
	case "process":
		// For process runtime, we need a process spec
		if s.Process == nil {
			return NewValidationError("process configuration is required for process runtime")
		}
		if err := s.Process.Validate(); err != nil {
			return WrapValidationError(err, "invalid process configuration")
		}
	default:
		return NewValidationError("unknown runtime: " + string(s.Runtime))
	}

	if s.Scale < 0 {
		return NewValidationError("service scale cannot be negative")
	}

	// Validate ports if present
	for i, port := range s.Ports {
		if port.Name == "" {
			return NewValidationError("port name is required for port at index " + strconv.Itoa(i))
		}
		if port.Port <= 0 || port.Port > 65535 {
			return NewValidationError("port must be between 1 and 65535 for port " + port.Name)
		}
	}

	// Validate health checks if present
	if s.Health != nil {
		if err := s.Health.Validate(); err != nil {
			return WrapValidationError(err, "invalid health check")
		}
	}

	// Validate network policy if present
	if s.NetworkPolicy != nil {
		if err := s.NetworkPolicy.Validate(); err != nil {
			return WrapValidationError(err, "invalid network policy")
		}
	}

	// Validate autoscale if present
	if s.Autoscale != nil && s.Autoscale.Enabled {
		if s.Autoscale.Min < 0 {
			return NewValidationError("autoscale min cannot be negative")
		}
		if s.Autoscale.Max < s.Autoscale.Min {
			return NewValidationError("autoscale max cannot be less than min")
		}
		if s.Autoscale.Metric == "" {
			return NewValidationError("autoscale metric is required")
		}
		if s.Autoscale.Target == "" {
			return NewValidationError("autoscale target is required")
		}
	}

	// Validate expose if present
	if s.Expose != nil {
		if s.Expose.Port == "" {
			return NewValidationError("expose port is required")
		}
	}

	// Validate volume mounts (RUNE-070).
	if err := ValidateVolumeMounts(s.Volumes); err != nil {
		return err
	}

	// Cross-mount lint: shared mountPaths, system-path blocklist, RWO+scale>1.
	owner := fmt.Sprintf("service %q", s.Name)
	if err := ValidateMountPathConflicts(owner, s.Scale, s.Volumes, s.SecretMounts, s.ConfigmapMounts); err != nil {
		return err
	}

	// Process-runtime services may only bind local host-path StorageClasses
	// via claimTemplate (RUNE-070).
	if err := ValidateProcessRuntimeVolumes(owner, s.Runtime, s.Volumes); err != nil {
		return err
	}

	// Init steps (RUNE-121).
	if err := s.validateInitSteps(); err != nil {
		return err
	}

	// SecurityContext (main container).
	if err := s.SecurityContext.Validate(); err != nil {
		return err
	}

	return nil
}

// CalculateHash hashes everything a controller acts on. A change moves
// Generation; a write that leaves Generation alone is treated as status-only
// and waits for the next resync.
func (s *Service) CalculateHash() string {
	return s.hash(true)
}

// CalculateTemplateHash hashes only what an instance is built from. A change
// moves TemplateGeneration, which marks every running instance outdated and
// replaces it, so a field belongs here only if a running instance cannot pick
// it up any other way.
func (s *Service) CalculateTemplateHash() string {
	return s.hash(false)
}

// serviceTemplate is what an instance is built from: the container or
// process config the runner creates it with, plus the probes, which the
// health monitor captures once when it starts watching an instance. The
// liveness failureThreshold is the exception, read live on every failure.
//
// Every field is omitempty so nil and empty compare equal: a spec that has
// been through the store comes back with nil where the caster sent [].
type serviceTemplate struct {
	Image           string            `json:"image,omitempty"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	EnvFrom         []EnvFromSource   `json:"envFrom,omitempty"`
	Runtime         RuntimeType       `json:"runtime,omitempty"`
	Process         *ProcessSpec      `json:"process,omitempty"`
	Ports           []ServicePort     `json:"ports,omitempty"`
	Resources       Resources         `json:"resources"`
	Health          *HealthCheck      `json:"health,omitempty"`
	SecretMounts    []SecretMount     `json:"secretMounts,omitempty"`
	ConfigmapMounts []ConfigmapMount  `json:"configmapMounts,omitempty"`
	Volumes         []VolumeMount     `json:"volumes,omitempty"`
	InitSteps       []InitStep        `json:"initSteps,omitempty"`
	SecurityContext *SecurityContext  `json:"securityContext,omitempty"`
}

// serviceDesiredState is what controllers read from the live service, so a
// change applies without replacing anything. Labels, the pull settings and
// expose are also copied onto an instance when it is created; those copies
// catch up at the next roll rather than forcing one.
type serviceDesiredState struct {
	Scale              int                      `json:"scale"`
	UpdateStrategy     *UpdateStrategy          `json:"updateStrategy,omitempty"`
	DrainSeconds       *int                     `json:"drainSeconds,omitempty"`
	ImageRegistry      string                   `json:"imageRegistry,omitempty"`
	Registry           *ServiceRegistryOverride `json:"registry,omitempty"`
	ImagePull          string                   `json:"imagePull,omitempty"`
	ImagePullAnonymous bool                     `json:"imagePullAnonymous,omitempty"`
	Labels             map[string]string        `json:"labels,omitempty"`
	RestartPolicy      RestartPolicy            `json:"restartPolicy,omitempty"`
	Expose             *ServiceExpose           `json:"expose,omitempty"`
	Dependencies       []DependencyRef          `json:"dependencies,omitempty"`
	NetworkPolicy      *ServiceNetworkPolicy    `json:"networkPolicy,omitempty"`
	Discovery          *ServiceDiscovery        `json:"discovery,omitempty"`
	LivenessFailures   int                      `json:"livenessFailures,omitempty"`
	// A claim template is read only when an ordinal's volume does not exist
	// yet; a replacement reattaches the existing one, so changing the
	// template affects new ordinals only.
	ClaimTemplates []VolumeMount `json:"claimTemplates,omitempty"`
	// Affinity has no consumer while every instance lands on the node that
	// creates it. Once a scheduler places instances, a changed affinity
	// means existing ones may be on the wrong node, and it may belong in
	// the template.
	Affinity  *ServiceAffinity  `json:"affinity,omitempty"`
	Autoscale *ServiceAutoscale `json:"autoscale,omitempty"`
}

func (s *Service) hash(includeDesiredState bool) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	// Neither view holds anything encoding/json can fail on, and a failure
	// would make the hash a constant that hides every change.
	if err := enc.Encode(s.templateView()); err != nil {
		panic(fmt.Sprintf("service template hash: %v", err))
	}
	if includeDesiredState {
		if err := enc.Encode(s.desiredStateView()); err != nil {
			panic(fmt.Sprintf("service desired-state hash: %v", err))
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (s *Service) templateView() serviceTemplate {
	res := s.Resources
	if g := res.GPU; g != nil {
		// A zero count means one device, so spelling out count: 1 must not
		// replace anything. A nil request and an empty one stay distinct:
		// no GPU versus one whole device.
		res.GPU = &GPURequest{Count: g.DeviceCount(), VRAM: g.VRAM, AllowHeterogeneous: g.AllowHeterogeneous}
	}
	return serviceTemplate{
		Image:           s.Image,
		Command:         s.Command,
		Args:            s.Args,
		Env:             s.Env,
		EnvFrom:         s.EnvFrom,
		Runtime:         s.Runtime,
		Process:         s.Process,
		Ports:           s.Ports,
		Resources:       res,
		Health:          healthWithoutLivenessThreshold(s.Health),
		SecretMounts:    sortedSecretMounts(s.SecretMounts),
		ConfigmapMounts: sortedConfigmapMounts(s.ConfigmapMounts),
		Volumes:         volumeMountShapes(s.Volumes),
		InitSteps:       s.InitSteps,
		SecurityContext: s.SecurityContext,
	}
}

func (s *Service) desiredStateView() serviceDesiredState {
	return serviceDesiredState{
		Scale:              s.Scale,
		UpdateStrategy:     s.UpdateStrategy,
		DrainSeconds:       s.DrainSeconds,
		ImageRegistry:      s.ImageRegistry,
		Registry:           s.Registry,
		ImagePull:          s.ImagePull,
		ImagePullAnonymous: s.ImagePullAnonymous,
		Labels:             s.Labels,
		RestartPolicy:      s.RestartPolicy,
		Expose:             s.Expose,
		Dependencies:       sortedDependencies(s.Dependencies),
		NetworkPolicy:      networkPolicyForHash(s.NetworkPolicy),
		Discovery:          discoveryWithoutVIP(s.Discovery),
		LivenessFailures:   livenessThreshold(s.Health),
		ClaimTemplates:     claimTemplates(s.Volumes),
		Affinity:           s.Affinity,
		Autoscale:          s.Autoscale,
	}
}

// networkPolicyForHash turns empty peer lists into nil. Their JSON has no
// omitempty, and the proto conversion returns [] where a cast spec has nil.
func networkPolicyForHash(np *ServiceNetworkPolicy) *ServiceNetworkPolicy {
	if np == nil || (len(np.Ingress) == 0 && len(np.Egress) == 0) {
		return nil
	}
	out := &ServiceNetworkPolicy{}
	for _, r := range np.Ingress {
		if len(r.From) == 0 {
			r.From = nil
		}
		out.Ingress = append(out.Ingress, r)
	}
	for _, r := range np.Egress {
		if len(r.To) == 0 {
			r.To = nil
		}
		out.Egress = append(out.Egress, r)
	}
	return out
}

func healthWithoutLivenessThreshold(h *HealthCheck) *HealthCheck {
	if h == nil || h.Liveness == nil {
		return h
	}
	out := *h
	live := *h.Liveness
	live.FailureThreshold = 0
	out.Liveness = &live
	return &out
}

func livenessThreshold(h *HealthCheck) int {
	if h == nil || h.Liveness == nil {
		return 0
	}
	return h.Liveness.FailureThreshold
}

// volumeMountShapes keeps whether a mount uses a claim template, but not the
// template itself.
func volumeMountShapes(mounts []VolumeMount) []VolumeMount {
	out := sortedVolumeMounts(mounts)
	for i := range out {
		if out[i].ClaimTemplate != nil {
			out[i].ClaimTemplate = &VolumeClaimTemplate{}
		}
	}
	return out
}

func claimTemplates(mounts []VolumeMount) []VolumeMount {
	var out []VolumeMount
	for _, m := range sortedVolumeMounts(mounts) {
		if m.ClaimTemplate != nil {
			out = append(out, VolumeMount{Name: m.Name, MountPath: m.MountPath, ClaimTemplate: m.ClaimTemplate})
		}
	}
	return out
}

// discoveryWithoutVIP drops the VIP, which the server assigns: a rendered
// spec never carries it, so hashing it would move Generation on every cast.
func discoveryWithoutVIP(d *ServiceDiscovery) *ServiceDiscovery {
	if d == nil {
		return nil
	}
	out := *d
	out.VIP = ""
	if out == (ServiceDiscovery{}) {
		return nil
	}
	return &out
}

func sortedKeyToPaths(items []KeyToPath) []KeyToPath {
	out := append([]KeyToPath(nil), items...)
	sort.Slice(out, func(a, b int) bool {
		if out[a].Key != out[b].Key {
			return out[a].Key < out[b].Key
		}
		return out[a].Path < out[b].Path
	})
	return out
}

func sortedSecretMounts(mounts []SecretMount) []SecretMount {
	out := make([]SecretMount, len(mounts))
	for i, m := range mounts {
		m.Items = sortedKeyToPaths(m.Items)
		out[i] = m
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].MountPath != out[j].MountPath {
			return out[i].MountPath < out[j].MountPath
		}
		return out[i].SecretName < out[j].SecretName
	})
	return out
}

func sortedConfigmapMounts(mounts []ConfigmapMount) []ConfigmapMount {
	out := make([]ConfigmapMount, len(mounts))
	for i, m := range mounts {
		m.Items = sortedKeyToPaths(m.Items)
		out[i] = m
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].MountPath != out[j].MountPath {
			return out[i].MountPath < out[j].MountPath
		}
		return out[i].ConfigmapName < out[j].ConfigmapName
	})
	return out
}

func sortedVolumeMounts(mounts []VolumeMount) []VolumeMount {
	out := append([]VolumeMount(nil), mounts...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].MountPath < out[j].MountPath
	})
	return out
}

func sortedDependencies(deps []DependencyRef) []DependencyRef {
	out := append([]DependencyRef(nil), deps...)
	prio := func(rt ResourceType) int {
		switch rt {
		case ResourceTypeService:
			return 0
		case ResourceTypeConfigmap:
			return 1
		case ResourceTypeSecret:
			return 2
		default:
			return 3
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		ti, tj := out[i].GetDependencyResourceType(), out[j].GetDependencyResourceType()
		if ti != tj {
			return prio(ti) < prio(tj)
		}
		return out[i].GetDependencyResourceName() < out[j].GetDependencyResourceName()
	})
	return out
}

// Validate validates the health check configuration.
func (h *HealthCheck) Validate() error {
	if h.Liveness != nil {
		if err := h.Liveness.Validate(); err != nil {
			return WrapValidationError(err, "invalid liveness probe")
		}
		// One pass clears a liveness probe's failures, as in Kubernetes. A
		// higher value was accepted and ignored before, so refuse it rather
		// than start restarting existing services more often.
		if h.Liveness.SuccessThreshold > 1 {
			return NewValidationError("invalid liveness probe: successThreshold must be 1; " +
				"it applies to readiness only")
		}
	}

	if h.Readiness != nil {
		if err := h.Readiness.Validate(); err != nil {
			return WrapValidationError(err, "invalid readiness probe")
		}
	}

	return nil
}

// Validate validates the probe configuration.
func (p *Probe) Validate() error {
	switch p.Type {
	case "http":
		if p.Path == "" {
			return NewValidationError("http probe must have a path")
		}
		if p.Port <= 0 {
			return NewValidationError("http probe must have a valid port")
		}
	case "tcp":
		if p.Port <= 0 {
			return NewValidationError("tcp probe must have a valid port")
		}
	case "exec":
		if len(p.Command) == 0 {
			return NewValidationError("exec probe must have a command")
		}
	default:
		return NewValidationError("unknown probe type: " + p.Type)
	}

	return nil
}
