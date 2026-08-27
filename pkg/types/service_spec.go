package types

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

var _ Spec = (*ServiceSpec)(nil)

// ServiceSpec is the YAML/JSON specification for a service.
type ServiceSpec struct {
	// Human-readable name for the service (required)
	Name string `json:"name" yaml:"name"`

	// Namespace the service belongs to (optional, defaults to "default")
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Labels for the service
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// Container image for the service (required)
	Image string `json:"image" yaml:"image"`

	// Command to run in the container (overrides image CMD)
	Command string `json:"command,omitempty" yaml:"command,omitempty"`

	// Arguments to the command
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`

	// Environment variables for the service
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// Import environment variables from secrets/configmaps with optional prefix
	EnvFrom EnvFromList `json:"envFrom,omitempty" yaml:"envFrom,omitempty"`

	// Number of instances to run (default: 1)
	Scale int `json:"scale" yaml:"scale"`

	// Ports exposed by the service
	Ports []ServicePort `json:"ports,omitempty" yaml:"ports,omitempty"`

	// Resource requirements for each instance
	Resources *Resources `json:"resources,omitempty" yaml:"resources,omitempty"`

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

	// Configmap mounts
	ConfigmapMounts []ConfigmapMount `json:"configmapMounts,omitempty" yaml:"configmapMounts,omitempty"`

	// Volume mounts. Each entry references either a pre-existing Volume by
	// name (Claim) or an inline ClaimTemplate the controller materializes
	// into a per-instance Volume.
	Volumes []VolumeMount `json:"volumes,omitempty" yaml:"volumes,omitempty"`

	// Service discovery configuration (operator-facing; no VIP field).
	Discovery *ServiceDiscoverySpec `json:"discovery,omitempty" yaml:"discovery,omitempty"`

	// Named registry selector for pulling the image (optional)
	ImageRegistry string `json:"imageRegistry,omitempty" yaml:"imageRegistry,omitempty"`

	// Registry override allowing inline auth or named selection (optional)
	Registry *ServiceRegistryOverride `json:"registry,omitempty" yaml:"registry,omitempty"`

	// ImagePull controls when the runner pulls the container image.
	// Allowed values: "always" (default), "missing", "never".
	ImagePull string `json:"imagePull,omitempty" yaml:"imagePull,omitempty"`

	// ImagePullAnonymous forces the pull to send no registry credentials,
	// even when a configured registry entry matches this image's host. For a
	// public image on a registry you also hold private credentials for.
	ImagePullAnonymous bool `json:"imagePullAnonymous,omitempty" yaml:"imagePullAnonymous,omitempty"`

	// Skip indicates this spec should be ignored by castfile parsing
	Skip bool `json:"skip,omitempty" yaml:"skip,omitempty"`

	// Dependencies in user-facing form. Accepts either:
	// - FQDN strings (e.g., "db.prod.rune") as YAML sequence entries
	// - ResourceRef strings (e.g., "secret:db-creds" or "configmap:app-settings")
	// - Structured objects (service/secret/configmap with optional namespace)
	// These will be normalized to []DependencyRef in internal Service
	Dependencies []ServiceDependency `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`

	// InitSteps run sequentially before each instance's main container (RUNE-121).
	InitSteps []InitStep `json:"initSteps,omitempty" yaml:"initSteps,omitempty"`

	// SecurityContext applied to the main container. Privileged and
	// seccomp=unconfined are admin-gated by the server.
	SecurityContext *SecurityContext `json:"securityContext,omitempty" yaml:"securityContext,omitempty"`

	// UpdateStrategy is how instances are replaced when the template changes
	// (RUNE-042). Accepts the string shorthand (`updateStrategy: recreate`)
	// or the map form. Omitted means rolling.
	UpdateStrategy *UpdateStrategy `json:"updateStrategy,omitempty" yaml:"updateStrategy,omitempty"`

	// DrainSeconds is the shutdown grace for this service's instances,
	// applied to every teardown (not just updates). Omitted means
	// DefaultDrainSeconds; floored at MinDrainSeconds.
	DrainSeconds *int `json:"drainSeconds,omitempty" yaml:"drainSeconds,omitempty"`

	// rawNode holds the original YAML mapping node for structural validation
	rawNode *yaml.Node `json:"-" yaml:"-"`
}

// EnvFromSourceSpec defines an import source for environment variables
type EnvFromSourceSpec struct {
	// One of these must be set
	Secret    string `json:"secret,omitempty" yaml:"secret,omitempty"`
	Configmap string `json:"configmap,omitempty" yaml:"configmap,omitempty"`

	// Optional namespace; defaults to the service namespace
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Optional prefix to apply to each imported key
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`

	// Raw holds the original scalar form (possibly a template placeholder) used during restoration
	Raw string `json:"-" yaml:"-"`
}

// EnvFromList allows envFrom to be either a single item (mapping or scalar) or a list in YAML
type EnvFromList []EnvFromSourceSpec

// UnmarshalYAML supports sequence, mapping, or scalar forms for envFrom
func (l *EnvFromList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		var items []EnvFromSourceSpec
		if err := value.Decode(&items); err != nil {
			return err
		}
		*l = EnvFromList(items)
		return nil
	case yaml.MappingNode, yaml.ScalarNode:
		var single EnvFromSourceSpec
		if err := value.Decode(&single); err != nil {
			return err
		}
		*l = EnvFromList{single}
		return nil
	default:
		return fmt.Errorf("invalid envFrom: expected sequence, mapping, or scalar")
	}
}

// UnmarshalYAML allows envFrom entries to be specified as either a mapping
// (secret/configmap/namespace/prefix) or a shorthand scalar like
// "{{secret:name}}", "secret:name", "config:app-settings" or FQDN forms.
func (e *EnvFromSourceSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.MappingNode:
		// Manually parse mapping keys to support flow-mapping and odd shapes
		var secret, config, ns, prefix string
		var walk func(n *yaml.Node)
		walk = func(n *yaml.Node) {
			for i := 0; i+1 < len(n.Content); i += 2 {
				keyNode := n.Content[i]
				valNode := n.Content[i+1]
				var k string
				if keyNode.Kind == yaml.ScalarNode {
					k = strings.ToLower(strings.TrimSpace(keyNode.Value))
				}
				var sval string
				switch valNode.Kind {
				case yaml.ScalarNode:
					sval = strings.TrimSpace(valNode.Value)
				case yaml.MappingNode:
					// Flatten single-pair mapping values like {value: x}
					if len(valNode.Content) >= 2 && valNode.Content[0].Kind == yaml.ScalarNode && valNode.Content[1].Kind == yaml.ScalarNode {
						sval = strings.TrimSpace(valNode.Content[1].Value)
					}
				}
				switch k {
				case "secret":
					secret = sval
				case "configmap":
					config = sval
				case "namespace":
					ns = sval
				case "prefix":
					prefix = sval
				default:
					if valNode.Kind == yaml.MappingNode {
						walk(valNode)
					}
				}
			}
		}
		walk(value)
		e.Secret = secret
		e.Configmap = config
		e.Namespace = ns
		e.Prefix = prefix
		return nil
	case yaml.ScalarNode:
		s := strings.TrimSpace(value.Value)
		e.Raw = s
		// Placeholder from preprocessTemplates: defer resolution
		if strings.HasPrefix(s, "__TEMPLATE_PLACEHOLDER_") {
			return nil
		}
		// Direct template braces form (when not preprocessed): parse now
		if strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") {
			inner := strings.TrimSpace(s[2 : len(s)-2])
			rr, err := ParseResourceRef(inner)
			if err != nil {
				return err
			}
			switch rr.Type {
			case ResourceTypeSecret:
				e.Secret = rr.Name
			case ResourceTypeConfigmap:
				e.Configmap = rr.Name
			default:
				return fmt.Errorf("envFrom shorthand must reference secret or configmap, got %s", rr.Type)
			}
			e.Namespace = rr.Namespace
			return nil
		}
		// Plain shorthand form (secret:name or config:name)
		rr, err := ParseResourceRef(s)
		if err != nil {
			// Keep raw; may be restored later in castfile flow
			return nil
		}
		switch rr.Type {
		case ResourceTypeSecret:
			e.Secret = rr.Name
		case ResourceTypeConfigmap:
			e.Configmap = rr.Name
		default:
			return fmt.Errorf("envFrom shorthand must reference secret or configmap, got %s", rr.Type)
		}
		e.Namespace = rr.Namespace
		return nil
	default:
		return fmt.Errorf("invalid envFrom entry: expected mapping or string")
	}
}

// ServiceRegistryOverride allows per-service registry selection or inline auth
type ServiceRegistryOverride struct {
	Name string        `json:"name,omitempty" yaml:"name,omitempty"`
	Auth *RegistryAuth `json:"auth,omitempty" yaml:"auth,omitempty"`
}

// RegistryAuth defines supported inline auth types
type RegistryAuth struct {
	Type     string `json:"type,omitempty" yaml:"type,omitempty"` // basic | token | ecr
	Username string `json:"username,omitempty" yaml:"username,omitempty"`
	Password string `json:"password,omitempty" yaml:"password,omitempty"`
	Token    string `json:"token,omitempty" yaml:"token,omitempty"`
	Region   string `json:"region,omitempty" yaml:"region,omitempty"`
}

func (r *ServiceRegistryOverride) Validate() error {
	if r == nil {
		return nil
	}
	if r.Auth != nil {
		switch strings.ToLower(r.Auth.Type) {
		case "basic":
			if r.Auth.Username == "" || r.Auth.Password == "" {
				return NewValidationError("basic auth requires username and password")
			}
		case "token":
			if r.Auth.Token == "" {
				return NewValidationError("token auth requires token")
			}
		case "ecr":
			// region is specified in runefile registries; inline override may include it
			// no hard requirement here for MVP
		case "":
			// allow empty (will fall back to name or host)
		default:
			return NewValidationError("unsupported auth type: " + r.Auth.Type)
		}
	}
	return nil
}

// Implement Spec interface for ServiceSpec
func (s *ServiceSpec) GetName() string      { return s.Name }
func (s *ServiceSpec) GetNamespace() string { return s.Namespace }
func (s *ServiceSpec) Kind() string         { return "Service" }

// ValidateStructure validates the YAML structure against the service specification
// Deprecated: ValidateStructure is no longer used; structural checks happen in Validate via validateStructureFromNode.
func (s *ServiceSpec) ValidateStructure(data []byte) error { return nil }

// Validate validates the service specification. Checks run in the order
// listed and the first failure is returned, so moving an entry changes which
// error a bad spec reports.
func (s *ServiceSpec) Validate() error {
	for _, check := range []func() error{
		s.validateStructureFromNode,
		s.validateNameAndImage,
		s.validateScaleAndUpdate,
		s.validatePorts,
		s.validateHealth,
		s.validateEnvFrom,
		s.validateNetworkPolicy,
		s.validateAutoscale,
		s.validateDependencies,
		s.validateResources,
		s.validateExpose,
		s.validateMounts,
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (s *ServiceSpec) validateNameAndImage() error {
	if s.Name == "" {
		return NewValidationError("service name is required")
	}
	if s.Image == "" {
		return NewValidationError("service image is required")
	}
	if s.Registry != nil {
		if err := s.Registry.Validate(); err != nil {
			return WrapValidationError(err, "invalid registry override")
		}
	}
	if s.ImagePull != "" {
		switch s.ImagePull {
		case ImagePullAlways, ImagePullMissing, ImagePullNever:
		default:
			return NewValidationError(fmt.Sprintf("invalid imagePull %q (allowed: always, missing, never)", s.ImagePull))
		}
	}
	return nil
}

func (s *ServiceSpec) validateScaleAndUpdate() error {
	if s.Scale < 0 {
		return NewValidationError("service scale cannot be negative")
	}

	if err := s.UpdateStrategy.Validate(); err != nil {
		return err
	}
	// Rules that need the service's scale and surge capability. The spec has
	// no runtime field — it is resolved later — so the process-runtime case is
	// caught by Service.Validate instead; volumes and hostPorts are knowable
	// here and are the common ones.
	if s.UpdateStrategy != nil && s.UpdateStrategy.MinServing != nil {
		probe := &Service{Scale: s.Scale, Ports: s.Ports, Volumes: s.Volumes}
		if err := s.UpdateStrategy.ValidateForService(probe); err != nil {
			return err
		}
	}

	if s.DrainSeconds != nil && (*s.DrainSeconds < 0 || *s.DrainSeconds > MaxDrainSeconds) {
		return NewValidationError(fmt.Sprintf(
			"drainSeconds must be between 0 and %d (0 is still floored at %ds so in-flight requests are not cut off)",
			MaxDrainSeconds, MinDrainSeconds))
	}
	return nil
}

func (s *ServiceSpec) validatePorts() error {
	for i, port := range s.Ports {
		if port.Name == "" {
			return NewValidationError("port name is required for port at index " + strconv.Itoa(i))
		}
		if port.Port <= 0 || port.Port > 65535 {
			return NewValidationError("port must be between 1 and 65535 for port " + port.Name)
		}
	}
	return nil
}

func (s *ServiceSpec) validateHealth() error {
	if s.Health == nil {
		return nil
	}
	if err := s.Health.Validate(); err != nil {
		return WrapValidationError(err, "invalid health check")
	}
	return nil
}

func (s *ServiceSpec) validateEnvFrom() error {
	for i, src := range s.EnvFrom {
		if (src.Secret == "" && src.Configmap == "") || (src.Secret != "" && src.Configmap != "") {
			return NewValidationError("envFrom item at index " + strconv.Itoa(i) + " must specify exactly one of 'secret' or 'configmap'")
		}
		if src.Secret != "" && strings.TrimSpace(src.Secret) == "" {
			return NewValidationError("envFrom.secret cannot be empty at index " + strconv.Itoa(i))
		}
		if src.Configmap != "" && strings.TrimSpace(src.Configmap) == "" {
			return NewValidationError("envFrom.configmap cannot be empty at index " + strconv.Itoa(i))
		}
		// Prefix can be any non-empty string; stricter validation enforced when materializing env vars
	}
	return nil
}

func (s *ServiceSpec) validateNetworkPolicy() error {
	if s.NetworkPolicy == nil {
		return nil
	}
	if err := s.NetworkPolicy.Validate(); err != nil {
		return WrapValidationError(err, "invalid network policy")
	}
	return nil
}

func (s *ServiceSpec) validateAutoscale() error {
	if s.Autoscale == nil || !s.Autoscale.Enabled {
		return nil
	}
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
	return nil
}

// validateDependencies is independent of autoscale.
func (s *ServiceSpec) validateDependencies() error {
	for i, dep := range s.Dependencies {
		if dep.Service == "" && dep.FQDN == "" && dep.Secret == "" && dep.Configmap == "" {
			return NewValidationError("dependency at index " + strconv.Itoa(i) + " must specify service, secret, or configmap (or FQDN string)")
		}
	}
	return nil
}

func (s *ServiceSpec) validateResources() error {
	if s.Resources == nil {
		return nil
	}
	if err := validateResourcePair("cpu", s.Resources.CPU.Request, s.Resources.CPU.Limit, ParseCPU); err != nil {
		return err
	}
	if err := validateResourcePair("memory", s.Resources.Memory.Request, s.Resources.Memory.Limit, ParseMemory); err != nil {
		return err
	}
	return s.validateGPU()
}

// validateResourcePair applies the three rules every request/limit pair shares:
// each side parses, neither is negative, and request does not exceed limit.
func validateResourcePair[T int64 | float64](field, request, limit string, parse func(string) (T, error)) error {
	req, err := parseResourceQuantity(field+".request", request, parse)
	if err != nil {
		return err
	}
	lim, err := parseResourceQuantity(field+".limit", limit, parse)
	if err != nil {
		return err
	}
	if request != "" && limit != "" && req > lim {
		return NewValidationError(field + ".request cannot exceed " + field + ".limit")
	}
	return nil
}

func parseResourceQuantity[T int64 | float64](field, value string, parse func(string) (T, error)) (T, error) {
	if value == "" {
		return 0, nil
	}
	v, err := parse(value)
	if err != nil {
		return 0, NewValidationError("invalid " + field + ": " + err.Error())
	}
	if v < 0 {
		return 0, NewValidationError(field + " cannot be negative")
	}
	return v, nil
}

func (s *ServiceSpec) validateExpose() error {
	if s.Expose == nil {
		return nil
	}
	if s.Expose.Port == "" {
		return NewValidationError("expose port is required")
	}
	resolved := s.resolveExposedPort()
	if resolved == nil {
		return NewValidationError("expose.port must reference a declared service port by name or number")
	}
	// Default protocol to tcp if empty
	proto := strings.ToLower(strings.TrimSpace(resolved.Protocol))
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" {
		return NewValidationError("expose only supports tcp protocol in MVP")
	}
	return nil
}

// resolveExposedPort matches expose.port against the declared ports by name
// first, then by number, and returns nil when neither matches.
func (s *ServiceSpec) resolveExposedPort() *ServicePort {
	for i := range s.Ports {
		p := &s.Ports[i]
		if p.Name == s.Expose.Port {
			return p
		}
		if n, err := strconv.Atoi(s.Expose.Port); err == nil && n == p.Port {
			return p
		}
	}
	return nil
}

func (s *ServiceSpec) validateMounts() error {
	// Validate volume mounts (RUNE-070).
	if err := ValidateVolumeMounts(s.Volumes); err != nil {
		return err
	}

	// Cross-mount lint (RUNE-070/072): shared paths, system blocklist, RWO+scale>1.
	owner := fmt.Sprintf("service %q", s.Name)
	if err := ValidateMountPathConflicts(owner, s.Scale, s.Volumes, s.SecretMounts, s.ConfigmapMounts); err != nil {
		return err
	}

	// Note: ValidateProcessRuntimeVolumes lives on Service.Validate, not
	// here — ServiceSpec is the user-facing castfile shape and does not
	// carry a Runtime field; runtime is resolved later when the spec is
	// converted to a Service.

	return nil
}

// validServiceFields is the set of keys accepted directly under `service:` in a
// cast file. It is a hand-maintained mirror of ServiceSpec's yaml tags: adding a
// field to the struct is NOT enough, it must be listed here too or cast rejects
// it as unknown. TestValidServiceFieldsMatchesSpec guards the two against drift
// (imagePullAnonymous shipped in v0.0.1-dev.139 with the struct field but not
// this list, so every cast using it failed validation).
var validServiceFields = map[string]bool{
	"name":               true,
	"namespace":          true,
	"labels":             true,
	"image":              true,
	"command":            true,
	"args":               true,
	"env":                true,
	"envFrom":            true,
	"scale":              true,
	"ports":              true,
	"resources":          true,
	"health":             true,
	"networkPolicy":      true,
	"expose":             true,
	"affinity":           true,
	"autoscale":          true,
	"secretMounts":       true,
	"configmapMounts":    true,
	"volumes":            true,
	"discovery":          true,
	"imageRegistry":      true,
	"registry":           true,
	"imagePull":          true,
	"imagePullAnonymous": true,
	"skip":               true,
	"dependencies":       true,
	"initSteps":          true,
	"securityContext":    true,
	"updateStrategy":     true,
	"drainSeconds":       true,
}

// validUpdateStrategyFields is the map-form allowlist for updateStrategy, so
// `updateStrategy: {typ: recreate}` is a cast error rather than a silently
// ignored typo that leaves the service on the default strategy.
var validUpdateStrategyFields = map[string]bool{
	"type":       true,
	"minServing": true,
}

// validResourcesFields and validGPUFields are the map-form allowlists for
// `resources:` and `resources.gpu:`. Without them the decoder's
// non-strict mode accepts an unknown sub-key and drops it, and for GPU
// that is not merely ignored — `gpu: {vrm: "20Gi"}` decodes to a non-nil
// GPURequest with an empty VRAM, which means ONE WHOLE DEVICE. A typo
// silently converts a 20Gi share into an exclusive claim on the card.
//
// `vendor` is listed as rejected-on-purpose rather than absent: the name
// is reserved for a future multi-vendor request, and the error should say
// so instead of reading like a typo.
var validResourcesFields = map[string]bool{
	"cpu":    true,
	"memory": true,
	"gpu":    true,
}

var validGPUFields = map[string]bool{
	"count":              true,
	"vram":               true,
	"allowHeterogeneous": true,
}

// validateStructureFromNode validates unknown fields using the captured raw YAML node.
// If no raw node is available (e.g., constructed programmatically), it is a no-op.
func (s *ServiceSpec) validateStructureFromNode() error {
	if s.rawNode == nil {
		return nil
	}

	validHealthFields := map[string]bool{
		"liveness":  true,
		"readiness": true,
	}
	validProbeFields := map[string]bool{
		"type":                true,
		"path":                true,
		"host":                true,
		"port":                true,
		"command":             true,
		"initialDelaySeconds": true,
		"intervalSeconds":     true,
		"timeoutSeconds":      true,
		"failureThreshold":    true,
		"successThreshold":    true,
	}
	validPortFields := map[string]bool{
		"name":       true,
		"port":       true,
		"targetPort": true,
		"protocol":   true,
		"hostPort":   true,
	}

	var errors []string
	// Validate fields directly on the service mapping node
	if s.rawNode.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(s.rawNode.Content); i += 2 {
			fieldKey := s.rawNode.Content[i]
			fieldVal := s.rawNode.Content[i+1]
			if !validServiceFields[fieldKey.Value] {
				errors = append(errors, fmt.Sprintf("unknown field '%s' in service specification at line %d", fieldKey.Value, fieldKey.Line))
				continue
			}
			if fieldKey.Value == "health" && fieldVal.Kind == yaml.MappingNode {
				collectServiceHealthErrors(fieldVal, validHealthFields, validProbeFields, &errors)
			}
			if fieldKey.Value == "ports" && fieldVal.Kind == yaml.SequenceNode {
				collectServicePortsErrors(fieldVal, validPortFields, &errors)
			}
			if fieldKey.Value == "securityContext" && fieldVal.Kind == yaml.MappingNode {
				collectSecurityContextErrors("service.securityContext", fieldVal, &errors)
			}
			if fieldKey.Value == "initSteps" && fieldVal.Kind == yaml.SequenceNode {
				collectInitStepsErrors(fieldVal, &errors)
			}
			if fieldKey.Value == "updateStrategy" && fieldVal.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(fieldVal.Content); j += 2 {
					k := fieldVal.Content[j]
					if !validUpdateStrategyFields[k.Value] {
						errors = append(errors, fmt.Sprintf(
							"unknown field '%s' in service.updateStrategy at line %d", k.Value, k.Line))
					}
				}
			}
			if fieldKey.Value == "resources" && fieldVal.Kind == yaml.MappingNode {
				collectResourcesErrors(fieldVal, &errors)
			}
			if fieldKey.Value == "discovery" && fieldVal.Kind == yaml.MappingNode {
				validDiscoveryFields := map[string]bool{
					"mode":               true,
					"localityPreference": true,
				}
				for j := 0; j+1 < len(fieldVal.Content); j += 2 {
					k := fieldVal.Content[j]
					if !validDiscoveryFields[k.Value] {
						errors = append(errors, fmt.Sprintf("unknown field '%s' in discovery at line %d (cluster VIP is assigned by Rune)", k.Value, k.Line))
					}
				}
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("validation errors:\n%s", strings.Join(errors, "\n"))
	}
	return nil
}

// ToService converts a ServiceSpec to a Service.
func (s *ServiceSpec) ToService() (*Service, error) {
	// Validate
	if err := s.Validate(); err != nil {
		return nil, err
	}

	// Set default namespace if not specified
	namespace := s.Namespace
	if namespace == "" {
		namespace = "default"
	}

	now := time.Now()

	var resources Resources
	if s.Resources != nil {
		resources = *s.Resources
	}

	// Normalize dependencies
	deps := make([]DependencyRef, 0, len(s.Dependencies))
	for _, d := range s.Dependencies {
		// FQDN string form: treat as service
		if d.FQDN != "" {
			parts := strings.Split(d.FQDN, ".")
			switch len(parts) {
			case 1:
				deps = append(deps, DependencyRef{Service: parts[0], Namespace: namespace})
			default:
				deps = append(deps, DependencyRef{Service: parts[0], Namespace: parts[1]})
			}
			continue
		}
		ns := d.Namespace
		if ns == "" {
			ns = namespace
		}
		if d.Service != "" {
			deps = append(deps, DependencyRef{Service: d.Service, Namespace: ns})
			continue
		}
		if d.Secret != "" {
			deps = append(deps, DependencyRef{Secret: d.Secret, Namespace: ns})
			continue
		}
		if d.Configmap != "" {
			deps = append(deps, DependencyRef{Configmap: d.Configmap, Namespace: ns})
			continue
		}
	}

	return &Service{
		ID:                 uuid.New().String(),
		Name:               s.Name,
		Namespace:          namespace,
		Labels:             s.Labels,
		Image:              s.Image,
		ImageRegistry:      s.ImageRegistry,
		Registry:           s.Registry,
		ImagePull:          s.ImagePull,
		ImagePullAnonymous: s.ImagePullAnonymous,
		Command:            s.Command,
		Args:               s.Args,
		Env:                s.Env,
		EnvFrom:            normalizeEnvFrom(namespace, s.EnvFrom),
		Scale:              s.Scale,
		Ports:              s.Ports,
		Resources:          resources,
		Health:             s.Health,
		NetworkPolicy:      s.NetworkPolicy,
		Expose:             s.Expose,
		Affinity:           s.Affinity,
		Autoscale:          s.Autoscale,
		SecretMounts:       s.SecretMounts,
		ConfigmapMounts:    s.ConfigmapMounts,
		Volumes:            s.Volumes,
		Discovery:          serviceDiscoveryFromSpec(s.Discovery),
		Dependencies:       deps,
		InitSteps:          s.InitSteps,
		SecurityContext:    s.SecurityContext,
		UpdateStrategy:     s.UpdateStrategy,
		DrainSeconds:       s.DrainSeconds,
		Status:             ServiceStatusPending,
		Metadata:           &ServiceMetadata{CreatedAt: now, UpdatedAt: now},
	}, nil
}

func serviceDiscoveryFromSpec(d *ServiceDiscoverySpec) *ServiceDiscovery {
	if d == nil {
		return nil
	}
	out := &ServiceDiscovery{
		Mode:               d.Mode,
		LocalityPreference: d.LocalityPreference,
	}
	if out.Mode == "" && out.LocalityPreference == "" {
		return nil
	}
	return out
}

// ServiceDependency is the spec-facing dependency format.
// YAML supports either string FQDN or this structured form per entry.
type ServiceDependency struct {
	// Optional raw FQDN captured by YAML parsing helpers
	FQDN string `json:"-" yaml:"-"`

	Service   string `json:"service,omitempty" yaml:"service,omitempty"`
	Secret    string `json:"secret,omitempty" yaml:"secret,omitempty"`
	Configmap string `json:"configmap,omitempty" yaml:"configmap,omitempty"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// UnmarshalYAML allows ServiceDependency entries to be specified as either
// a plain string (FQDN or resource ref) or a structured object.
func (d *ServiceDependency) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		// String form: could be FQDN service or resource ref
		s := strings.TrimSpace(value.Value)
		// Try resource ref first
		rr, err := ParseResourceRef(s)
		if err == nil {
			// Fill from resource ref
			switch rr.Type {
			case ResourceTypeService:
				d.Service = rr.Name
			case ResourceTypeSecret:
				d.Secret = rr.Name
			case ResourceTypeConfigmap:
				d.Configmap = rr.Name
			default:
				// unknown - treat as raw FQDN service
				d.FQDN = s
			}
			d.Namespace = rr.Namespace
			return nil
		}
		// Not a resource ref; treat as FQDN service string
		d.FQDN = s
		d.Service = ""
		d.Namespace = ""
		return nil
	case yaml.MappingNode:
		// Structured form
		type depAlias struct {
			Service   string `yaml:"service"`
			Secret    string `yaml:"secret"`
			Configmap string `yaml:"configmap"`
			Namespace string `yaml:"namespace"`
		}
		var a depAlias
		if err := value.Decode(&a); err != nil {
			return err
		}
		d.FQDN = ""
		d.Service = a.Service
		d.Secret = a.Secret
		d.Configmap = a.Configmap
		d.Namespace = a.Namespace
		return nil
	default:
		return fmt.Errorf("invalid dependency format: expected string or mapping")
	}
}

// RestoreTemplateReferences restores template references in environment variables
// This should be called after parsing to restore the original template syntax
func (s *ServiceSpec) RestoreTemplateReferences(templateMap map[string]string) {
	if s.Env == nil {
		return
	}

	for key, value := range s.Env {
		// Create a working copy of the value
		restoredValue := value

		// Replace all placeholders in this value
		for placeholder, templateRef := range templateMap {
			if strings.Contains(restoredValue, placeholder) {
				restoredValue = strings.ReplaceAll(restoredValue, placeholder, "{{"+templateRef+"}}")
			}
		}

		// Update the environment variable with the restored value
		s.Env[key] = restoredValue
	}
}

// GetEnvWithTemplates returns environment variables with template references restored
func (s *ServiceSpec) GetEnvWithTemplates(templateMap map[string]string) map[string]string {
	if s.Env == nil {
		return nil
	}

	result := make(map[string]string)
	for key, value := range s.Env {
		// Create a working copy of the value
		restoredValue := value

		// Replace all placeholders in this value
		for placeholder, templateRef := range templateMap {
			if strings.Contains(restoredValue, placeholder) {
				restoredValue = strings.ReplaceAll(restoredValue, placeholder, "{{"+templateRef+"}}")
			}
		}

		result[key] = restoredValue
	}
	return result
}

// RestoreEnvFrom resolves any template placeholders captured in EnvFrom.Raw
// using the provided templateMap, and populates Secret/Configmap/Namespace accordingly.
func (s *ServiceSpec) RestoreEnvFrom(templateMap map[string]string) {
	if len(s.EnvFrom) == 0 {
		return
	}
	for i := range s.EnvFrom {
		src := &s.EnvFrom[i]
		if (src.Secret != "" || src.Configmap != "") || src.Raw == "" {
			continue
		}
		raw := strings.TrimSpace(src.Raw)
		// If this is a placeholder, map it back to template content
		if strings.HasPrefix(raw, "__TEMPLATE_PLACEHOLDER_") {
			if ref, ok := templateMap[raw]; ok {
				raw = ref
			}
		}
		// Strip braces if present
		if strings.HasPrefix(raw, "{{") && strings.HasSuffix(raw, "}}") {
			raw = strings.TrimSpace(raw[2 : len(raw)-2])
		}
		rr, err := ParseResourceRef(raw)
		if err != nil {
			continue
		}
		switch rr.Type {
		case ResourceTypeSecret:
			src.Secret = rr.Name
		case ResourceTypeConfigmap:
			src.Configmap = rr.Name
		default:
			// ignore unsupported
		}
		if rr.Namespace != "" {
			src.Namespace = rr.Namespace
		}
	}
}

// collectValidationErrors recursively collects validation errors for YAML structure
// Deprecated: collectServiceValidationErrors is no longer used.

// collectHealthErrors collects validation errors for health check structure
func collectServiceHealthErrors(healthNode *yaml.Node, validHealthFields map[string]bool, validProbeFields map[string]bool, errors *[]string) {
	for i := 0; i < len(healthNode.Content); i += 2 {
		key := healthNode.Content[i]
		value := healthNode.Content[i+1]

		if !validHealthFields[key.Value] {
			*errors = append(*errors, fmt.Sprintf("unknown field '%s' in health check specification at line %d", key.Value, key.Line))
		}

		// Validate probe structure
		if value.Kind == yaml.MappingNode {
			for j := 0; j < len(value.Content); j += 2 {
				probeKey := value.Content[j]
				if !validProbeFields[probeKey.Value] {
					*errors = append(*errors, fmt.Sprintf("unknown field '%s' in probe specification at line %d", probeKey.Value, probeKey.Line))
				}
			}
		}
	}
}

// collectPortsErrors collects validation errors for ports structure
func collectServicePortsErrors(portsNode *yaml.Node, validPortFields map[string]bool, errors *[]string) {
	for _, portNode := range portsNode.Content {
		if portNode.Kind == yaml.MappingNode {
			for i := 0; i < len(portNode.Content); i += 2 {
				key := portNode.Content[i]
				if !validPortFields[key.Value] {
					*errors = append(*errors, fmt.Sprintf("unknown field '%s' in port specification at line %d", key.Value, key.Line))
				}
			}
		}
	}
}

// validSecurityContextFields lists the recognised keys inside a
// `securityContext` block. Mirrors types.SecurityContext.
var validSecurityContextFields = map[string]bool{
	"seccompProfile": true,
	"capAdd":         true,
	"capDrop":        true,
	"privileged":     true,
}

// validSeccompProfileFields lists the recognised keys inside a
// `seccompProfile` block. Mirrors types.SeccompProfile.
var validSeccompProfileFields = map[string]bool{
	"type":             true,
	"localhostProfile": true,
}

// securityContextFieldHints maps common wrong field names to the
// correct schema so the validator can append "did you mean" hints.
// Keep these focused on the mistakes we've actually seen in the wild
// — over-eager fuzzy matching produces confusing suggestions.
var securityContextFieldHints = map[string]string{
	"seccomp":            "seccompProfile.type",
	"seccompProfileType": "seccompProfile.type",
	"capabilities":       "capAdd / capDrop",
	"caps":               "capAdd / capDrop",
}

// collectSecurityContextErrors walks a securityContext mapping node
// and reports unknown fields. The block is parsed as a strict
// allowlist because silent yaml.Unmarshal drops would otherwise mask
// schema typos until runtime (Propeller hit this on `seccomp:
// unconfined` vs `seccompProfile: { type: unconfined }`).
func collectSecurityContextErrors(ctx string, node *yaml.Node, errors *[]string) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		val := node.Content[i+1]
		if !validSecurityContextFields[key.Value] {
			msg := fmt.Sprintf("unknown field '%s' in %s at line %d", key.Value, ctx, key.Line)
			if hint, ok := securityContextFieldHints[key.Value]; ok {
				msg += fmt.Sprintf(" (did you mean '%s'?)", hint)
			}
			*errors = append(*errors, msg)
			continue
		}
		if key.Value == "seccompProfile" && val.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(val.Content); j += 2 {
				k := val.Content[j]
				if !validSeccompProfileFields[k.Value] {
					*errors = append(*errors, fmt.Sprintf("unknown field '%s' in %s.seccompProfile at line %d", k.Value, ctx, k.Line))
				}
			}
		}
	}
}

// validInitStepFields lists the recognised keys for an init step
// entry. Mirrors types.InitStep YAML tags.
var validInitStepFields = map[string]bool{
	"name":            true,
	"image":           true,
	"command":         true,
	"args":            true,
	"env":             true,
	"envFrom":         true,
	"volumes":         true,
	"secretMounts":    true,
	"configmapMounts": true,
	"resources":       true,
	"runIf":           true,
	"timeout":         true,
	"restartPolicy":   true,
	"securityContext": true,
}

// validRunIfFields lists the recognised keys for an init step's
// runIf predicate. Mirrors types.RunIf.
var validRunIfFields = map[string]bool{
	"type":   true,
	"path":   true,
	"volume": true,
}

// collectInitStepsErrors validates each init step's top-level fields
// and recurses into its securityContext and runIf blocks.
func collectInitStepsErrors(stepsNode *yaml.Node, errors *[]string) {
	for idx, step := range stepsNode.Content {
		if step.Kind != yaml.MappingNode {
			continue
		}
		ctx := fmt.Sprintf("initSteps[%d]", idx)
		for i := 0; i+1 < len(step.Content); i += 2 {
			key := step.Content[i]
			val := step.Content[i+1]
			if !validInitStepFields[key.Value] {
				*errors = append(*errors, fmt.Sprintf("unknown field '%s' in %s at line %d", key.Value, ctx, key.Line))
				continue
			}
			if key.Value == "securityContext" && val.Kind == yaml.MappingNode {
				collectSecurityContextErrors(ctx+".securityContext", val, errors)
			}
			if key.Value == "runIf" && val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					k := val.Content[j]
					if !validRunIfFields[k.Value] {
						*errors = append(*errors, fmt.Sprintf("unknown field '%s' in %s.runIf at line %d", k.Value, ctx, k.Line))
					}
				}
			}
		}
	}
}

// normalizeEnvFrom converts EnvFromList to []EnvFromSource with default namespace applied.
func normalizeEnvFrom(defaultNS string, sources EnvFromList) []EnvFromSource {
	if len(sources) == 0 {
		return nil
	}
	out := make([]EnvFromSource, 0, len(sources))
	for _, src := range sources {
		ns := src.Namespace
		if ns == "" {
			ns = defaultNS
		}
		n := EnvFromSource{Namespace: ns, Prefix: src.Prefix}
		if src.Secret != "" {
			n.SecretName = src.Secret
		}
		if src.Configmap != "" {
			n.ConfigmapName = src.Configmap
		}
		out = append(out, n)
	}
	return out
}

// validateGPU checks resources.gpu. These are Validate() errors rather
// than lints on purpose: the server's create path calls neither .Lint()
// nor .Validate() directly, so a lint is a client-side courtesy a
// scripted cast can skip — and a rule that stops a tenant from voiding
// the GPU ledger cannot be optional.
func (s *ServiceSpec) validateGPU() error {
	g := s.Resources.GPU
	if g == nil {
		return nil
	}

	if g.Count < 0 {
		return NewValidationError("resources.gpu.count cannot be negative")
	}
	if g.VRAM != "" {
		v, err := ParseMemory(g.VRAM)
		if err != nil {
			return NewValidationError("invalid resources.gpu.vram: " + err.Error())
		}
		if v <= 0 {
			return NewValidationError("resources.gpu.vram must be greater than zero")
		}
	}
	// Multi-device is whole-device only. Splitting a vram request across
	// several cards has no consumer and makes per-device accounting
	// ambiguous — is 20Gi the total, or 20Gi on each?
	if g.Count > 1 && g.VRAM != "" {
		return NewValidationError(
			"resources.gpu: count > 1 cannot be combined with vram — multi-device requests take whole devices")
	}
	if g.AllowHeterogeneous && g.Count <= 1 {
		return NewValidationError(
			"resources.gpu.allowHeterogeneous is meaningful only with count > 1")
	}

	// A GPU request is a statement about accounting; a privileged
	// container is a statement that accounting does not apply. privileged
	// bind-mounts host /dev and grants the device cgroup c *:* rwm, so
	// the runtime's device scoping is void and the container sees every
	// card on the box regardless of what it reserved.
	if sc := s.SecurityContext; sc != nil {
		switch {
		case sc.Privileged:
			return NewValidationError(
				"resources.gpu cannot be combined with securityContext.privileged: " +
					"a privileged container sees every GPU on the host, so the reservation would not hold")
		case len(sc.CapAdd) > 0:
			return NewValidationError(
				"resources.gpu cannot be combined with securityContext.capAdd: " +
					"added capabilities can reach devices outside the reservation")
		case sc.SeccompProfile != nil && sc.SeccompProfile.Type.Canonical() == SeccompProfileUnconfined:
			return NewValidationError(
				"resources.gpu cannot be combined with seccompProfile: unconfined: " +
					"an unconfined profile can reach devices outside the reservation")
		}
	}

	// Rune sets these to scope the container to its assigned devices.
	// A spec that sets them too would either be overridden silently or
	// override Rune — and NVIDIA_VISIBLE_DEVICES=all in particular hands
	// the container every card on the box.
	for _, k := range []string{"CUDA_VISIBLE_DEVICES", "NVIDIA_VISIBLE_DEVICES"} {
		if _, ok := s.Env[k]; ok {
			return NewValidationError(
				"resources.gpu cannot be combined with a " + k + " entry in env: " +
					"Rune sets it from the device assignment")
		}
	}
	return nil
}

// collectResourcesErrors drills into `resources:` and `resources.gpu:`.
// The decoder is non-strict, so anything not listed is dropped silently —
// see validResourcesFields for why that is worse than usual under gpu.
func collectResourcesErrors(node *yaml.Node, errors *[]string) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if !validResourcesFields[k.Value] {
			*errors = append(*errors, fmt.Sprintf(
				"unknown field '%s' in service.resources at line %d", k.Value, k.Line))
			continue
		}
		if k.Value != "gpu" || v.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(v.Content); j += 2 {
			gk := v.Content[j]
			if validGPUFields[gk.Value] {
				continue
			}
			if gk.Value == "vendor" {
				*errors = append(*errors, fmt.Sprintf(
					"'vendor' in service.resources.gpu at line %d is reserved and not yet accepted "+
						"— NVIDIA is the only supported vendor; select a node with a gpu.vendor label instead",
					gk.Line))
				continue
			}
			*errors = append(*errors, fmt.Sprintf(
				"unknown field '%s' in service.resources.gpu at line %d", gk.Value, gk.Line))
		}
	}
}
