package types

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	yaml "gopkg.in/yaml.v3"
)

// SecretSpec represents the YAML specification for a secret.
type SecretSpec struct {
	// Human-readable name for the secret (required)
	Name string `json:"name" yaml:"name"`

	// Namespace the secret belongs to (optional, defaults to "default")
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Type of secret (static or dynamic)
	Type string `json:"type" yaml:"type"`

	// Secret data for static secrets (only for creation, not returned in GET)
	Data map[string]string `json:"data,omitempty" yaml:"data,omitempty"`

	// Single value convenience field (alternative to data.value for simple secrets)
	Value string `json:"value,omitempty" yaml:"value,omitempty"`

	// Base64-encoded value (for binary data)
	ValueBase64 string `json:"valueBase64,omitempty" yaml:"valueBase64,omitempty"`

	// Engine configuration for dynamic secrets
	Engine *SecretEngine `json:"engine,omitempty" yaml:"engine,omitempty"`

	// Rotation configuration
	Rotation *RotationPolicy `json:"rotation,omitempty" yaml:"rotation,omitempty"`

	// Skip indicates this spec should be ignored by castfile parsing
	Skip bool `json:"skip,omitempty" yaml:"skip,omitempty"`

	// rawNode holds the original YAML mapping node for structural validation
	rawNode *yaml.Node `json:"-" yaml:"-"`
}

// UnmarshalYAML implements custom unmarshalling so `data` can be provided
// either as a mapping (key: value) or as a sequence of {key, value} objects.
func (s *SecretSpec) UnmarshalYAML(value *yaml.Node) error {
	// Preserve the original node for structural validation
	s.rawNode = value

	// Decode known fields, but keep `data` as a raw node so we can post-process
	var aux struct {
		Name        string          `yaml:"name"`
		Namespace   string          `yaml:"namespace"`
		Type        string          `yaml:"type"`
		Data        interface{}     `yaml:"data"`
		Value       string          `yaml:"value"`
		ValueBase64 string          `yaml:"valueBase64"`
		Engine      *SecretEngine   `yaml:"engine"`
		Rotation    *RotationPolicy `yaml:"rotation"`
	}
	if err := value.Decode(&aux); err != nil {
		return err
	}

	s.Name = aux.Name
	s.Namespace = aux.Namespace
	s.Type = aux.Type
	// Allow environment variable expansion in scalar fields
	s.Value = os.ExpandEnv(aux.Value)
	s.ValueBase64 = os.ExpandEnv(aux.ValueBase64)
	s.Engine = aux.Engine
	s.Rotation = aux.Rotation

	// Normalize data into map[string]string, accepting both map and list forms
	if aux.Data != nil {
		normalized, err := decodeStringMapOrKVAny(aux.Data)
		if err != nil {
			return err
		}
		// Expand environment variables in all data values
		for k, v := range normalized {
			normalized[k] = os.ExpandEnv(v)
		}
		s.Data = normalized
	}

	return nil
}

// decodeStringMapOrKVList converts a YAML node into map[string]string accepting:
// - mapping: {k: v, ...}
// - sequence: - { key: k, value: v }
func decodeStringMapOrKVList(node *yaml.Node) (map[string]string, error) {
	result := make(map[string]string)
	switch node.Kind {
	case yaml.MappingNode:
		// Decode directly to map[string]string
		var m map[string]string
		if err := node.Decode(&m); err != nil {
			return nil, err
		}
		for k, v := range m {
			result[k] = v
		}
	case yaml.SequenceNode:
		// Expect list items shaped like {key: ..., value: ...}
		for _, item := range node.Content {
			if item.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("invalid data item: expected mapping, got %v", item.Kind)
			}
			var entry struct {
				Key   string `yaml:"key"`
				Value string `yaml:"value"`
			}
			if err := item.Decode(&entry); err != nil {
				return nil, err
			}
			if entry.Key == "" {
				return nil, fmt.Errorf("invalid data entry missing 'key'")
			}
			result[entry.Key] = entry.Value
		}
	case 0:
		// Absent / null
		return result, nil
	default:
		return nil, fmt.Errorf("invalid data format: expected mapping or sequence")
	}
	return result, nil
}

// decodeStringMapOrKVAny handles cases where `data` was decoded into a generic
// interface{} by yaml.v3 before we normalize it into map[string]string.
func decodeStringMapOrKVAny(v interface{}) (map[string]string, error) {
	switch t := v.(type) {
	case *yaml.Node:
		return decodeStringMapOrKVList(t)
	case map[string]interface{}:
		out := make(map[string]string, len(t))
		for k, val := range t {
			switch vv := val.(type) {
			case string:
				out[k] = vv
			case fmt.Stringer:
				out[k] = vv.String()
			default:
				return nil, fmt.Errorf("invalid data value for key %q: must be string", k)
			}
		}
		return out, nil
	case map[string]string:
		return t, nil
	case []interface{}:
		out := make(map[string]string)
		for _, it := range t {
			m, ok := it.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("invalid data item: expected mapping")
			}
			keyVal, ok1 := m["key"]
			valVal, ok2 := m["value"]
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("invalid data entry missing 'key' or 'value'")
			}
			key, ok := keyVal.(string)
			if !ok || key == "" {
				return nil, fmt.Errorf("invalid data entry missing 'key'")
			}
			switch vv := valVal.(type) {
			case string:
				out[key] = vv
			case fmt.Stringer:
				out[key] = vv.String()
			default:
				return nil, fmt.Errorf("invalid data value for key %q: must be string", key)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("invalid data format: expected mapping or sequence")
	}
}

// Implement Spec interface for SecretSpec
func (s *SecretSpec) GetName() string      { return s.Name }
func (s *SecretSpec) GetNamespace() string { return s.Namespace }
func (s *SecretSpec) Kind() string         { return "Secret" }

// ToSecret converts a SecretSpec to a Secret.
func (s *SecretSpec) ToSecret() (*Secret, error) {
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

	// Create the secret
	secret := &Secret{
		ID:        uuid.New().String(),
		Name:      s.Name,
		Namespace: namespace,
		Type:      s.Type,
		Engine:    s.Engine,
		Rotation:  s.Rotation,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// For static secrets, set the data map
	if s.Type == "static" {
		data := make(map[string]string)

		// Start with any values in the data map
		for k, v := range s.Data {
			data[k] = v
		}

		// If value field is set, add it
		if s.Value != "" {
			data["value"] = s.Value
		}

		// If valueBase64 field is set, add it
		// In a real implementation, we'd decode this and store the raw value
		if s.ValueBase64 != "" {
			data["valueBase64"] = s.ValueBase64
		}

		secret.Data = data
	}

	return secret, nil
}

// Validate checks if a secret specification is valid.
func (s *SecretSpec) Validate() error {
	// Structural validation against original YAML node when available
	if err := s.validateStructureFromNode(); err != nil {
		return err
	}
	if s.Name == "" {
		return NewValidationError("secret name is required")
	}

	if s.Type == "" {
		s.Type = "static"
	}

	if s.Type != "static" && s.Type != "dynamic" {
		return NewValidationError("secret type must be 'static' or 'dynamic'")
	}

	switch s.Type {
	case "static":
		// For static secrets, either data, value, or valueBase64 must be provided
		if len(s.Data) == 0 && s.Value == "" && s.ValueBase64 == "" {
			return NewValidationError("static secret must have data, value, or valueBase64")
		}
	case "dynamic":
		// For dynamic secrets, engine must be specified
		if s.Engine == nil {
			return NewValidationError("dynamic secret must have an engine configuration")
		}

		if s.Engine.Name == "" {
			return NewValidationError("dynamic secret engine name is required")
		}
	}

	return nil
}

// RestoreTemplateReferences restores template references in secret data values.
// Called by the castfile parser so cast-time templating in `data:` values
// (e.g. `{{ secret:db-host/value }}`) survives YAML parsing. The literal
// `{{ ... }}` strings are substituted at cast time by the cast-secret renderer.
func (s *SecretSpec) RestoreTemplateReferences(templateMap map[string]string) {
	if len(templateMap) == 0 {
		return
	}
	restore := func(in string) string {
		out := in
		for placeholder, templateRef := range templateMap {
			if strings.Contains(out, placeholder) {
				out = strings.ReplaceAll(out, placeholder, "{{"+templateRef+"}}")
			}
		}
		return out
	}
	for k, v := range s.Data {
		s.Data[k] = restore(v)
	}
	if s.Value != "" {
		s.Value = restore(s.Value)
	}
	if s.ValueBase64 != "" {
		s.ValueBase64 = restore(s.ValueBase64)
	}
}

// validateStructureFromNode validates unknown fields using the captured raw YAML node.
// If no raw node is available (e.g., constructed programmatically), it is a no-op.
func (s *SecretSpec) validateStructureFromNode() error {
	if s.rawNode == nil {
		return nil
	}

	validFields := map[string]bool{
		"name":        true,
		"namespace":   true,
		"type":        true,
		"data":        true,
		"value":       true,
		"valueBase64": true,
		"engine":      true,
		"rotation":    true,
		"skip":        true,
	}

	var errors []string
	if s.rawNode.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(s.rawNode.Content); i += 2 {
			k := s.rawNode.Content[i]
			if !validFields[k.Value] {
				errors = append(errors, fmt.Sprintf("unknown field '%s' in secret specification at line %d", k.Value, k.Line))
			}
		}
	}
	if len(errors) > 0 {
		return NewValidationError(strings.Join(errors, "\n"))
	}
	return nil
}

// SecretsEngineSpec represents the YAML specification for a secrets engine.
type SecretsEngineSpec struct {
	// Engine metadata and configuration
	SecretsEngine struct {
		// Human-readable name for the engine (required)
		Name string `json:"name" yaml:"name"`

		// Namespace the engine belongs to (optional, defaults to "default")
		Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

		// Type of engine (required: builtin, function, plugin)
		Type string `json:"type" yaml:"type"`

		// For function-based engines, the reference to the function
		Function string `json:"function,omitempty" yaml:"function,omitempty"`

		// For plugin-based engines, the path to the plugin executable
		Path string `json:"path,omitempty" yaml:"path,omitempty"`

		// Engine-wide configuration
		Config map[string]interface{} `json:"config,omitempty" yaml:"config,omitempty"`
	} `json:"secretsEngine" yaml:"secretsEngine"`
}

// Validate checks if a secrets engine specification is valid.
func (s *SecretsEngineSpec) Validate() error {
	if s.SecretsEngine.Name == "" {
		return NewValidationError("secrets engine name is required")
	}

	if s.SecretsEngine.Type == "" {
		return NewValidationError("secrets engine type is required")
	}

	// Type-specific validation
	switch s.SecretsEngine.Type {
	case "function":
		if s.SecretsEngine.Function == "" {
			return NewValidationError("function-based secrets engine must specify a function")
		}
	case "plugin":
		if s.SecretsEngine.Path == "" {
			return NewValidationError("plugin-based secrets engine must specify a path")
		}
	case "builtin":
		// No additional validation for builtin engines
	default:
		return NewValidationError("unknown secrets engine type: " + s.SecretsEngine.Type)
	}

	return nil
}

// ToSecretsEngine converts a SecretsEngineSpec to a SecretsEngine.
func (s *SecretsEngineSpec) ToSecretsEngine() (*SecretsEngine, error) {
	// Validate
	if err := s.Validate(); err != nil {
		return nil, err
	}

	// Set default namespace if not specified
	namespace := s.SecretsEngine.Namespace
	if namespace == "" {
		namespace = "default"
	}

	now := time.Now()

	return &SecretsEngine{
		ID:        uuid.New().String(),
		Name:      s.SecretsEngine.Name,
		Namespace: namespace,
		Type:      s.SecretsEngine.Type,
		Function:  s.SecretsEngine.Function,
		Path:      s.SecretsEngine.Path,
		Config:    s.SecretsEngine.Config,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}
