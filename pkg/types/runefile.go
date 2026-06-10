package types

import (
	"fmt"
	"os"
	filepathpkg "path/filepath"
	"strings"
	"time"

	tomlv2 "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// isTOMLPath returns true when the file path's extension indicates a
// TOML runefile. Anything else (including empty / unknown extensions)
// is treated as YAML by the parser, which preserves backward
// compatibility with the original Release-1 behaviour.
func isTOMLPath(p string) bool {
	return strings.EqualFold(filepathpkg.Ext(p), ".toml")
}

// readRunefileBytes loads `path` and, if it's a TOML file, transcodes
// it to YAML so that the existing yaml.v3 AST pipeline (line tracking,
// duplicate-key handling, top-level key validation, struct unmarshal)
// can be reused unchanged. Returns the YAML bytes plus a flag noting
// whether transcoding happened (so callers can decide whether to
// surface line numbers).
func readRunefileBytes(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	if !isTOMLPath(path) {
		return data, false, nil
	}
	var raw map[string]any
	if err := tomlv2.Unmarshal(data, &raw); err != nil {
		return nil, true, fmt.Errorf("failed to parse TOML: %w", err)
	}
	ydata, err := yaml.Marshal(raw)
	if err != nil {
		return nil, true, fmt.Errorf("failed to transcode TOML to YAML: %w", err)
	}
	return ydata, true, nil
}

// RuneFile represents a Rune configuration file.
//
// MUST stay shape-compatible with internal/config.Config — runed loads
// the runefile via viper into Config; lint loads it here. The two
// structs are kept parallel by hand and policed by a reflection-based
// parity test (see runefile_parity_test.go, RUNE-112). When you add
// a field to one, add it to the other (and to isKnownField below).
type RuneFile struct {
	// Server configuration
	Server *ServerConfig `yaml:"server,omitempty"`

	// Data directory for persistent storage
	DataDir string `yaml:"data_dir,omitempty"`

	// Client configuration
	Client *ClientConfig `yaml:"client,omitempty"`

	// Docker runner configuration (including private registries)
	Docker *DockerConfig `yaml:"docker,omitempty"`

	// Default namespace
	Namespace string `yaml:"namespace,omitempty"`

	// Authentication configuration
	Auth *AuthConfig `yaml:"auth,omitempty"`

	// Resource limits and requests
	Resources *ResourceConfig `yaml:"resources,omitempty"`

	// Logging configuration
	Log *LogConfig `yaml:"log,omitempty"`

	// Secret encryption + limits configuration
	Secret *SecretConfig `yaml:"secret,omitempty"`

	// Top-level config-resource limits (RUNE-016)
	Config *ConfigResourceConfig `yaml:"config,omitempty"`

	// Plugin configuration
	Plugins *PluginConfig `yaml:"plugins,omitempty"`

	// Networking layer (RUNE-040..067)
	Networking *NetworkingConfig `yaml:"networking,omitempty"`

	// Telemetry / metrics
	Telemetry *TelemetryConfig `yaml:"telemetry,omitempty"`

	// Node placement (edge / worker / leader)
	Node *NodeConfig `yaml:"node,omitempty"`

	// Ingress (edge node terminating user traffic)
	Ingress *IngressConfig `yaml:"ingress,omitempty"`

	// ACME (Let's Encrypt) configuration
	ACME *ACMEConfig `yaml:"acme,omitempty"`

	// Storage drivers. Per-driver opaque config maps keyed
	// by registry name (e.g. "local", "local-host"). The shape of
	// each inner map is driver-specific; the loader keeps it as raw
	// `any` and hands it to the driver factory.
	Storage *StorageConfig `yaml:"storage,omitempty"`

	// FailedInstanceRetention bounds how long preserved Failed-instance
	// containers (tombstones) survive before the reconciler's retention
	// GC reaps them. Mirrors internal/config.FailedInstanceRetention.
	FailedInstanceRetention *FailedInstanceRetentionConfig `yaml:"failed_instance_retention,omitempty"`

	// UI configures the embedded web dashboard (RUNE-200). Mirrors
	// internal/config.UI.
	UI *UIConfig `yaml:"ui,omitempty"`

	// Observability configures the native observability subsystem
	// (RuneSight): the agent forwarder, the log store backend, and
	// retention. Mirrors internal/config.Observability. When absent or
	// disabled, runed forwards no logs and `rune logs` falls back to the
	// live ephemeral stream.
	Observability *ObservabilityConfig `yaml:"observability,omitempty"`

	// Internal tracking for line numbers (not serialized)
	lineInfo map[string]int `json:"-" yaml:"-"`
	rawNode  *yaml.Node     `json:"-" yaml:"-"`
}

// UIConfig is the runefile-side view of the embedded dashboard settings
// (RUNE-200). Parallel to internal/config.UI.
type UIConfig struct {
	// Enabled turns the dashboard (and the HTTP serving layer) on/off.
	Enabled bool `yaml:"enabled,omitempty"`
	// Path is the dashboard mount point (default /ui).
	Path string `yaml:"path,omitempty"`
	// HandoffEnabled allows the `rune ui` token-handoff flow.
	HandoffEnabled bool `yaml:"handoff_enabled,omitempty"`
	// HandoffTTL bounds a one-time handoff code's lifetime.
	HandoffTTL time.Duration `yaml:"handoff_ttl,omitempty"`
	// RequireTLS refuses to serve the UI over plaintext on a non-loopback
	// address (binds 127.0.0.1 only when TLS is off).
	RequireTLS bool `yaml:"require_tls,omitempty"`
}

// FailedInstanceRetentionConfig is the runefile-side view of the failed-
// instance retention policy. Parallel to internal/config.FailedInstanceRetention.
type FailedInstanceRetentionConfig struct {
	// PerServiceCap is the max number of Failed tombstones kept per
	// service before the oldest is evicted. 0 disables the cap.
	PerServiceCap int `yaml:"per_service_cap,omitempty"`
	// TTL is the max age of a Failed tombstone before it is evicted
	// regardless of cap. 0 disables TTL.
	TTL time.Duration `yaml:"ttl,omitempty"`
	// SnapshotLogBytes caps the per-instance log snapshot captured into
	// Instance.LastLogs at eviction. 0 disables snapshotting. Reserved
	// for v2 — not yet honoured by the eviction path.
	SnapshotLogBytes int `yaml:"snapshot_log_bytes,omitempty"`
}

func (rf *RuneFile) GetName() string {
	return "rune"
}

// ServerConfig represents server endpoint configuration
type ServerConfig struct {
	GRPCAddress string     `yaml:"grpc_address,omitempty"`
	HTTPAddress string     `yaml:"http_address,omitempty"`
	TLS         *TLSConfig `yaml:"tls,omitempty"`
}

// TLSConfig represents TLS configuration
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

// ClientConfig represents client configuration
type ClientConfig struct {
	Timeout time.Duration `yaml:"timeout,omitempty"`
	Retries int           `yaml:"retries,omitempty"`
}

// DockerConfig represents Docker runner configuration
type DockerConfig struct {
	APIVersion                string                 `yaml:"api_version,omitempty"`
	FallbackAPIVersion        string                 `yaml:"fallback_api_version,omitempty"`
	NegotiationTimeoutSeconds int                    `yaml:"negotiation_timeout_seconds,omitempty"`
	LogMaxSize                string                 `yaml:"log_max_size,omitempty"`
	LogMaxFile                int                    `yaml:"log_max_file,omitempty"`
	Registries                []DockerRegistryConfig `yaml:"registries,omitempty"`
}

// DockerRegistryConfig is a single private-registry entry. Mirrors
// internal/config.DockerRegistryConfig.
type DockerRegistryConfig struct {
	Name     string             `yaml:"name"`
	Registry string             `yaml:"registry"`
	Auth     DockerRegistryAuth `yaml:"auth,omitempty"`
}

// DockerRegistryAuth holds registry credentials (or a fromSecret
// reference). Mirrors internal/config.DockerRegistryAuth — see
// RUNE-018 for the secret-bootstrap semantics.
type DockerRegistryAuth struct {
	Type       string            `yaml:"type,omitempty"` // basic | token | ecr
	Username   string            `yaml:"username,omitempty"`
	Password   string            `yaml:"password,omitempty"`
	Token      string            `yaml:"token,omitempty"`
	Region     string            `yaml:"region,omitempty"`
	FromSecret any               `yaml:"fromSecret,omitempty"` // string or {name,namespace}
	Bootstrap  bool              `yaml:"bootstrap,omitempty"`
	Manage     string            `yaml:"manage,omitempty"`
	Immutable  bool              `yaml:"immutable,omitempty"`
	Data       map[string]string `yaml:"data,omitempty"`
}

// AuthConfig represents authentication configuration
type AuthConfig struct {
	APIKeys          string `yaml:"api_keys,omitempty"`
	Provider         string `yaml:"provider,omitempty"`
	Token            string `yaml:"token,omitempty"`
	AllowRemoteAdmin bool   `yaml:"allow_remote_admin,omitempty"`
}

// ResourceConfig represents resource limits and requests
type ResourceConfig struct {
	CPU    *CPUConfig    `yaml:"cpu,omitempty"`
	Memory *MemoryConfig `yaml:"memory,omitempty"`
}

// CPUConfig represents CPU resource configuration
type CPUConfig struct {
	DefaultRequest string `yaml:"default_request,omitempty"`
	DefaultLimit   string `yaml:"default_limit,omitempty"`
}

// MemoryConfig represents memory resource configuration
type MemoryConfig struct {
	DefaultRequest string `yaml:"default_request,omitempty"`
	DefaultLimit   string `yaml:"default_limit,omitempty"`
}

// LogConfig represents logging configuration
type LogConfig struct {
	Level  string `yaml:"level,omitempty"`
	Format string `yaml:"format,omitempty"`
}

// SecretConfig represents secret encryption + storage limits configuration
type SecretConfig struct {
	Encryption *EncryptionConfig `yaml:"encryption,omitempty"`
	Limits     *LimitsConfig     `yaml:"limits,omitempty"`
}

// ConfigResourceConfig holds limits for the top-level `config:`
// resource type (RUNE-016 ConfigMaps). Distinct from `secret.limits`.
type ConfigResourceConfig struct {
	Limits *LimitsConfig `yaml:"limits,omitempty"`
}

// LimitsConfig mirrors store.Limits as it appears in the runefile.
// Field tags use snake_case for consistency with the rest of the file
// even though store.Limits itself is untagged (viper's default
// behaviour is to lowercase field names with no separator, but in
// practice runefiles in the wild use snake_case here — keep both
// shapes valid).
type LimitsConfig struct {
	MaxObjectBytes   int `yaml:"max_object_bytes,omitempty"`
	MaxKeyNameLength int `yaml:"max_key_name_length,omitempty"`
}

// EncryptionConfig represents encryption configuration
type EncryptionConfig struct {
	Enabled bool       `yaml:"enabled"`
	KEK     *KEKConfig `yaml:"kek,omitempty"`
}

// KEKConfig represents Key Encryption Key configuration
type KEKConfig struct {
	Source     string            `yaml:"source,omitempty"`
	File       string            `yaml:"file,omitempty"`
	Passphrase *PassphraseConfig `yaml:"passphrase,omitempty"`
}

// PassphraseConfig represents passphrase configuration
type PassphraseConfig struct {
	Enabled bool   `yaml:"enabled"`
	Env     string `yaml:"env,omitempty"`
}

// PluginConfig represents plugin configuration
type PluginConfig struct {
	Dir     string   `yaml:"dir,omitempty"`
	Enabled []string `yaml:"enabled,omitempty"`
}

// NetworkingConfig holds the networking-layer settings (RUNE-040..067).
type NetworkingConfig struct {
	ClusterCIDR string `yaml:"cluster_cidr,omitempty"`
	DevMode     bool   `yaml:"dev_mode,omitempty"`
}

// TelemetryConfig holds the metrics endpoint configuration.
type TelemetryConfig struct {
	MetricsAddr string `yaml:"metrics_addr,omitempty"`
}

// NodeConfig holds per-node placement metadata (edge / worker / leader).
type NodeConfig struct {
	Role string `yaml:"role,omitempty"`
}

// IngressConfig holds the bind addresses for the edge ingress (RUNE-067).
type IngressConfig struct {
	HTTPAddr  string `yaml:"http_addr,omitempty"`
	HTTPSAddr string `yaml:"https_addr,omitempty"`
}

// ACMEConfig holds the Let's Encrypt directory + contact email used by
// the edge ingress (RUNE-067).
type ACMEConfig struct {
	Directory string `yaml:"directory,omitempty"`
	Email     string `yaml:"email,omitempty"`
}

// StorageConfig mirrors internal/config.Storage. The shape
// of each per-driver inner map is opaque from the runefile's
// perspective: drivers register a factory + a parseConfig that
// validates whatever keys they care about. Keeping the value type as
// `any` lets us add new drivers without touching the runefile parser.
//
// Example:
//
//	storage:
//	  defaultStorageClass: local
//	  preserveOnDelete: false
//	  allowCreateMissing: false
//	  drivers:
//	    local:
//	      localVolumeRoot: /var/lib/rune/volumes
//	    local-host:
//	      hostPathAllowlist:
//	        - /srv/data
type StorageConfig struct {
	// DefaultStorageClass — see internal/config.Storage.DefaultStorageClass.
	DefaultStorageClass *string `yaml:"defaultStorageClass,omitempty"`

	// PreserveOnDelete — see internal/config.Storage.PreserveOnDelete.
	PreserveOnDelete bool `yaml:"preserveOnDelete,omitempty"`

	// AllowCreateMissing — see internal/config.Storage.AllowCreateMissing.
	AllowCreateMissing bool `yaml:"allowCreateMissing,omitempty"`

	Drivers map[string]map[string]any `yaml:"drivers,omitempty"`
}

// ObservabilityConfig is the runefile-side view of the native observability
// (RuneSight) subsystem. Parallel to internal/config.Observability.
//
// Example:
//
//	observability:
//	  enabled: true
//	  backend: embedded        # embedded | clickhouse | loki
//	  retention_days: 7
//	  loki:                    # only for backend: loki
//	    url: http://loki:3100
//	    tenant_id: ""
//	  clickhouse:              # only for backend: clickhouse
//	    dsn: clickhouse://user:pass@host:9000/runesight
//	    auto_migrate: true
//	    storage_policy: ""     # enables S3 tiering when set
//	    hot_days: 7
type ObservabilityConfig struct {
	// Enabled turns the forwarder + log store on. Default false (opt-in):
	// the embedded store is lightweight but logging every workload line is
	// an operator choice, so a bare install stays on the live ephemeral
	// stream until [observability] is switched on.
	Enabled bool `yaml:"enabled,omitempty"`

	// Backend selects the log store: embedded (default, in-process), or the
	// optional clickhouse / loki sinks.
	Backend string `yaml:"backend,omitempty"`

	// RetentionDays bounds record age: the embedded store's sweep, and the
	// ClickHouse table's DELETE TTL. 0 uses the backend default. Loki manages
	// its own retention server-side.
	RetentionDays int `yaml:"retention_days,omitempty"`

	// Loki configures the loki backend. Parallel to internal/config.LokiConfig.
	Loki *LokiConfig `yaml:"loki,omitempty"`

	// ClickHouse configures the clickhouse backend, including S3 tiering.
	// Parallel to internal/config.ClickHouseConfig.
	ClickHouse *ClickHouseConfig `yaml:"clickhouse,omitempty"`
}

// LokiConfig is the runefile block for the loki observability backend. Loki
// is object-storage-native (no tiering knobs here — point Loki itself at a
// bucket); this block only says where Loki is.
type LokiConfig struct {
	// URL is the Loki HTTP endpoint (e.g. http://loki:3100).
	URL string `yaml:"url,omitempty"`
	// TenantID is sent as X-Scope-OrgID for multi-tenant Loki.
	TenantID string `yaml:"tenant_id,omitempty"`
}

// ClickHouseConfig is the runefile block for the clickhouse observability
// backend. ClickHouse is local-disk-first; long retention is S3 tiering — a
// TTL move-to-volume against a server-configured storage policy (the policy
// and its S3 credentials live in ClickHouse server config, not here).
type ClickHouseConfig struct {
	// DSN is the connection string (clickhouse://user:pass@host:9000/db).
	DSN string `yaml:"dsn,omitempty"`
	// Database is the target database (default "runesight").
	Database string `yaml:"database,omitempty"`
	// Table is the target log table (default "logs").
	Table string `yaml:"table,omitempty"`
	// AutoMigrate creates the database/table on first connect (default true).
	AutoMigrate bool `yaml:"auto_migrate,omitempty"`
	// StoragePolicy is the server-configured policy naming the hot + s3
	// volumes. Empty disables tiering.
	StoragePolicy string `yaml:"storage_policy,omitempty"`
	// S3Volume is the volume within StoragePolicy aged parts move to
	// (default "s3").
	S3Volume string `yaml:"s3_volume,omitempty"`
	// HotDays moves parts older than this to S3Volume. 0 keeps everything
	// on the hot disk.
	HotDays int `yaml:"hot_days,omitempty"`
}

// ParseRuneFile parses a Rune configuration file from the given file
// path. Both YAML and TOML are accepted (TOML is detected via the
// `.toml` extension and transcoded to YAML before parsing — see
// readRunefileBytes for the rationale). Line numbers reported by the
// linter for TOML files refer to positions in the post-transcode YAML,
// not the original TOML source; callers that care about original-file
// positions should fall back to printing the key name only.
func ParseRuneFile(filePath string) (*RuneFile, error) {
	data, fromTOML, err := readRunefileBytes(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		if fromTOML {
			return nil, fmt.Errorf("failed to parse TOML (post-transcode): %w", err)
		}
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Validate top-level keys
	if err := validateRuneFileTopLevelKeys(&node); err != nil {
		return nil, fmt.Errorf("invalid top-level keys: %w", err)
	}

	var runeFile RuneFile
	if err := yaml.Unmarshal(data, &runeFile); err != nil {
		return nil, fmt.Errorf("failed to unmarshal RuneFile: %w", err)
	}

	// Store the raw node for validation
	runeFile.rawNode = &node

	// Initialize line info map
	runeFile.lineInfo = make(map[string]int)
	collectLineInfo(&node, runeFile.lineInfo)

	return &runeFile, nil
}

// validateRuneFileTopLevelKeys validates that all top-level keys are known
func validateRuneFileTopLevelKeys(node *yaml.Node) error {
	if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return fmt.Errorf("invalid YAML document structure")
	}

	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("root must be a mapping")
	}

	knownKeys := map[string]bool{
		"server":                    true,
		"data_dir":                  true,
		"client":                    true,
		"docker":                    true,
		"namespace":                 true,
		"auth":                      true,
		"resources":                 true,
		"log":                       true,
		"secret":                    true,
		"config":                    true,
		"plugins":                   true,
		"networking":                true,
		"telemetry":                 true,
		"node":                      true,
		"ingress":                   true,
		"acme":                      true,
		"storage":                   true,
		"failed_instance_retention": true,
		"observability":             true,
	}

	for i := 0; i < len(root.Content); i += 2 {
		if i+1 >= len(root.Content) {
			break
		}
		key := root.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		keyName := key.Value
		if !knownKeys[keyName] {
			return fmt.Errorf("unknown top-level key '%s' at line %d", keyName, key.Line)
		}
	}

	return nil
}

// collectLineInfo collects line numbers for all keys in the YAML document
func collectLineInfo(node *yaml.Node, lineInfo map[string]int) {
	if node == nil {
		return
	}

	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if i+1 >= len(node.Content) {
				break
			}
			key := node.Content[i]
			value := node.Content[i+1]
			if key.Kind == yaml.ScalarNode {
				lineInfo[key.Value] = key.Line
			}
			collectLineInfo(value, lineInfo)
		}
	} else if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			collectLineInfo(item, lineInfo)
		}
	}
}

// Validate validates the RuneFile configuration
func (rf *RuneFile) Validate() error {
	var errors []string

	// Validate server configuration
	if rf.Server != nil {
		if err := rf.validateServer(); err != nil {
			errors = append(errors, fmt.Sprintf("server: %v", err))
		}
	}

	// Validate client configuration
	if rf.Client != nil {
		if err := rf.validateClient(); err != nil {
			errors = append(errors, fmt.Sprintf("client: %v", err))
		}
	}

	// Validate docker configuration
	if rf.Docker != nil {
		if err := rf.validateDocker(); err != nil {
			errors = append(errors, fmt.Sprintf("docker: %v", err))
		}
	}

	// Validate auth configuration
	if rf.Auth != nil {
		if err := rf.validateAuth(); err != nil {
			errors = append(errors, fmt.Sprintf("auth: %v", err))
		}
	}

	// Validate resources configuration
	if rf.Resources != nil {
		if err := rf.validateResources(); err != nil {
			errors = append(errors, fmt.Sprintf("resources: %v", err))
		}
	}

	// Validate log configuration
	if rf.Log != nil {
		if err := rf.validateLog(); err != nil {
			errors = append(errors, fmt.Sprintf("log: %v", err))
		}
	}

	// Validate secret configuration
	if rf.Secret != nil {
		if err := rf.validateSecret(); err != nil {
			errors = append(errors, fmt.Sprintf("secret: %v", err))
		}
	}

	// Validate plugins configuration
	if rf.Plugins != nil {
		if err := rf.validatePlugins(); err != nil {
			errors = append(errors, fmt.Sprintf("plugins: %v", err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("validation failed:\n%s", strings.Join(errors, "\n"))
	}

	return nil
}

// Lint performs validation and returns a list of errors with line numbers
func (rf *RuneFile) Lint() []error {
	var errors []error

	// Validate the configuration
	if err := rf.Validate(); err != nil {
		// Split the error message and create individual errors with line numbers
		lines := strings.Split(err.Error(), "\n")
		for _, line := range lines {
			if strings.Contains(line, ":") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					key := strings.TrimSpace(parts[0])
					message := strings.TrimSpace(parts[1])

					// Get line number for this key
					lineNum := rf.lineInfo[key]
					if lineNum > 0 {
						errors = append(errors, fmt.Errorf("line %d: %s: %s", lineNum, key, message))
					} else {
						errors = append(errors, fmt.Errorf("%s: %s", key, message))
					}
				}
			}
		}
	}

	// Validate structure (unknown fields)
	if rf.rawNode != nil {
		if err := rf.validateStructureFromNode(); err != nil {
			errors = append(errors, err)
		}
	}

	return errors
}

// validateStructureFromNode validates that no unknown fields are present
func (rf *RuneFile) validateStructureFromNode() error {
	if rf.rawNode == nil {
		return nil
	}

	var errors []string
	validateNodeStructure(rf.rawNode, &errors)

	if len(errors) > 0 {
		return fmt.Errorf("structure validation failed:\n%s", strings.Join(errors, "\n"))
	}

	return nil
}

// validateNodeStructure recursively validates YAML node structure
func validateNodeStructure(node *yaml.Node, errors *[]string) {
	if node == nil {
		return
	}

	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if i+1 >= len(node.Content) {
				break
			}
			key := node.Content[i]
			value := node.Content[i+1]

			if key.Kind == yaml.ScalarNode {
				// Check if this is a known field at this level
				if !isKnownField(key.Value, node) {
					*errors = append(*errors, fmt.Sprintf("unknown field '%s' in rune configuration at line %d", key.Value, key.Line))
				}
			}
			validateNodeStructure(value, errors)
		}
	} else if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			validateNodeStructure(item, errors)
		}
	}
}

// isKnownField checks if a field name is known at the current YAML level
func isKnownField(fieldName string, node *yaml.Node) bool {
	// This is a simplified check - in a real implementation, you'd want to track
	// the current path and check against the appropriate struct definition
	knownKeys := map[string]bool{
		// Top-level sections
		"server":     true,
		"data_dir":   true,
		"client":     true,
		"docker":     true,
		"namespace":  true,
		"auth":       true,
		"resources":  true,
		"log":        true,
		"secret":     true,
		"config":     true,
		"plugins":    true,
		"networking": true,
		"telemetry":  true,
		"node":       true,
		"ingress":    true,
		"acme":       true,
		"storage":    true,

		// failed-instance retention (preserve failed containers for
		// postmortem; per-service cap + TTL)
		"failed_instance_retention": true,
		"per_service_cap":           true,
		"snapshot_log_bytes":        true,

		// embedded dashboard (RUNE-200): ui.{enabled,path,handoff_enabled,
		// handoff_ttl,require_tls}. "enabled" is shared with server.tls.
		"ui":              true,
		"path":            true,
		"handoff_enabled": true,
		"handoff_ttl":     true,
		"require_tls":     true,

		// native observability (RuneSight): observability.{enabled,backend,
		// retention_days, loki.{url,tenant_id}, clickhouse.{dsn,database,
		// table,auto_migrate,storage_policy,s3_volume,hot_days}}.
		// "enabled" is shared with other blocks.
		"observability":  true,
		"backend":        true,
		"retention_days": true,
		"loki":           true,
		"url":            true,
		"tenant_id":      true,
		"clickhouse":     true,
		"dsn":            true,
		"database":       true,
		"table":          true,
		"auto_migrate":   true,
		"storage_policy": true,
		"s3_volume":      true,
		"hot_days":       true,

		// storage.* (typed knobs + opaque per-driver maps; key names
		// under .drivers are driver-specific so we accept anything
		// underneath).
		"defaultstorageclass": true,
		"preserveondelete":    true,
		"allowcreatemissing":  true,
		"drivers":             true,

		// server / client
		"grpc_address": true,
		"http_address": true,
		"tls":          true,
		"enabled":      true,
		"cert_file":    true,
		"key_file":     true,
		"timeout":      true,
		"retries":      true,

		// docker (+ registries)
		"api_version":                 true,
		"fallback_api_version":        true,
		"negotiation_timeout_seconds": true,
		"registries":                  true,
		"name":                        true,
		"registry":                    true,
		"username":                    true,
		"password":                    true,
		"region":                      true,
		"fromSecret":                  true,
		"bootstrap":                   true,
		"manage":                      true,
		"immutable":                   true,
		"data":                        true,
		"type":                        true,

		// auth
		"api_keys":           true,
		"provider":           true,
		"token":              true,
		"allow_remote_admin": true,

		// resources
		"cpu":             true,
		"memory":          true,
		"default_request": true,
		"default_limit":   true,

		// log
		"level":  true,
		"format": true,

		// secret / config / limits
		"encryption":          true,
		"kek":                 true,
		"source":              true,
		"file":                true,
		"passphrase":          true,
		"env":                 true,
		"limits":              true,
		"max_object_bytes":    true,
		"max_key_name_length": true,

		// plugins
		"dir": true,

		// networking
		"cluster_cidr": true,
		"dev_mode":     true,

		// telemetry
		"metrics_addr": true,

		// node
		"role": true,

		// ingress
		"http_addr":  true,
		"https_addr": true,

		// acme
		"directory": true,
		"email":     true,
	}

	return knownKeys[fieldName]
}

// Individual validation methods
func (rf *RuneFile) validateServer() error {
	if rf.Server.GRPCAddress == "" && rf.Server.HTTPAddress == "" {
		return fmt.Errorf("at least one of grpc_address or http_address must be specified")
	}
	return nil
}

func (rf *RuneFile) validateClient() error {
	if rf.Client.Timeout < 0 {
		return fmt.Errorf("timeout cannot be negative")
	}
	if rf.Client.Retries < 0 {
		return fmt.Errorf("retries cannot be negative")
	}
	return nil
}

func (rf *RuneFile) validateDocker() error {
	if rf.Docker.NegotiationTimeoutSeconds < 0 {
		return fmt.Errorf("negotiation_timeout_seconds cannot be negative")
	}
	return nil
}

func (rf *RuneFile) validateAuth() error {
	if rf.Auth.Provider != "" && rf.Auth.Provider != "token" && rf.Auth.Provider != "oidc" && rf.Auth.Provider != "none" {
		return fmt.Errorf("provider must be one of: token, oidc, none")
	}
	return nil
}

func (rf *RuneFile) validateResources() error {
	// Add resource validation logic here
	return nil
}

func (rf *RuneFile) validateLog() error {
	if rf.Log.Level != "" {
		validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
		if !validLevels[rf.Log.Level] {
			return fmt.Errorf("log level must be one of: debug, info, warn, error")
		}
	}
	return nil
}

func (rf *RuneFile) validateSecret() error {
	if rf.Secret.Encryption != nil && rf.Secret.Encryption.Enabled {
		if rf.Secret.Encryption.KEK == nil {
			return fmt.Errorf("kek configuration is required when encryption is enabled")
		}
		if rf.Secret.Encryption.KEK.Source == "" {
			return fmt.Errorf("kek source is required when encryption is enabled")
		}
		if rf.Secret.Encryption.KEK.Source != "file" && rf.Secret.Encryption.KEK.Source != "env" && rf.Secret.Encryption.KEK.Source != "generated" {
			return fmt.Errorf("kek source must be one of: file, env, generated")
		}
		if rf.Secret.Encryption.KEK.Source == "file" && rf.Secret.Encryption.KEK.File == "" {
			return fmt.Errorf("kek file path is required when source is 'file'")
		}
	}
	return nil
}

func (rf *RuneFile) validatePlugins() error {
	// Add plugin validation logic here
	return nil
}

// GetLineInfo returns the line number for a given key
func (rf *RuneFile) GetLineInfo(key string) (int, bool) {
	line, exists := rf.lineInfo[key]
	return line, exists
}

// IsRuneConfigFile checks if a file appears to be a Rune configuration
// file. Accepts either YAML or TOML (TOML is detected by the `.toml`
// extension and transcoded to YAML internally so we can reuse the same
// AST-based key-counting heuristic).
func IsRuneConfigFile(filePath string) (bool, error) {
	data, _, err := readRunefileBytes(filePath)
	if err != nil {
		return false, err
	}

	// Use YAML AST to avoid duplicate-key unmarshal errors
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return false, err
	}
	// Navigate to document root mapping
	if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return false, nil
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return false, nil
	}

	runeKeys := map[string]bool{
		"server":    true,
		"client":    true,
		"auth":      true,
		"secret":    true,
		"plugins":   true,
		"docker":    true,
		"log":       true,
		"namespace": true,
		"resources": true,
	}

	count := 0
	for i := 0; i+1 < len(root.Content); i += 2 {
		k := root.Content[i]
		if k.Kind == yaml.ScalarNode {
			if runeKeys[k.Value] {
				count++
			}
		}
	}
	return count >= 2, nil
}
