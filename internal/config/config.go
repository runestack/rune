package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/runestack/rune/pkg/crypto"
	"github.com/runestack/rune/pkg/store"
	"github.com/spf13/viper"
)

var (
	// DefaultGRPCPort is the default gRPC port for Rune (T9 keypad for RUNE -> 7863)
	DefaultGRPCPort = 7863
	// DefaultHTTPPort is the default HTTP port for Rune (T9 keypad for RUNE -> 7861)
	DefaultHTTPPort = 7861
)

type TLS struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type Server struct {
	GRPCAddr string `yaml:"grpc_address"`
	HTTPAddr string `yaml:"http_address"`
	TLS      TLS    `yaml:"tls"`
}

// UI configures the embedded web dashboard (RUNE-200). The dashboard is
// served from the HTTP server on Server.HTTPAddr under Path. When disabled
// the HTTP server is not started at all (no /ui, no /grpc transcoder).
type UI struct {
	// Enabled turns the dashboard (and the HTTP serving layer it rides
	// on) on or off. Defaults to true.
	Enabled bool `yaml:"enabled"`
	// Path is the mount point for the dashboard. Defaults to "/ui".
	Path string `yaml:"path"`
	// HandoffEnabled allows the `rune ui login` CLI token-handoff flow
	// (POST /v1/ui/handoff/{code}). Defaults to true.
	HandoffEnabled bool `yaml:"handoff_enabled"`
	// HandoffTTL bounds the lifetime of a one-time handoff code.
	// Defaults to 60s.
	HandoffTTL time.Duration `yaml:"handoff_ttl"`
	// RequireTLS refuses to serve the UI over plaintext on a non-loopback
	// address: when true and TLS is off, the HTTP server binds 127.0.0.1
	// only and logs a warning. Defaults to true.
	RequireTLS bool `yaml:"require_tls"`
}

type Client struct {
	Timeout time.Duration `yaml:"timeout"`
	Retries int           `yaml:"retries"`
}

type Docker struct {
	APIVersion                string                 `yaml:"api_version"`
	FallbackAPIVersion        string                 `yaml:"fallback_api_version"`
	NegotiationTimeoutSeconds int                    `yaml:"negotiation_timeout_seconds"`
	Registries                []DockerRegistryConfig `yaml:"registries"`
}

// DockerRegistryConfig represents a registry entry in the runefile
type DockerRegistryConfig struct {
	Name     string             `yaml:"name"`
	Registry string             `yaml:"registry"`
	Auth     DockerRegistryAuth `yaml:"auth"`
}

// DockerRegistryAuth holds authentication configuration for a registry
type DockerRegistryAuth struct {
	Type       string            `yaml:"type"` // basic | token | ecr
	Username   string            `yaml:"username"`
	Password   string            `yaml:"password"`
	Token      string            `yaml:"token"`
	Region     string            `yaml:"region"`
	FromSecret any               `yaml:"fromSecret"` // string or {name,namespace}
	Bootstrap  bool              `yaml:"bootstrap"`
	Manage     string            `yaml:"manage"` // create|update|ignore
	Immutable  bool              `yaml:"immutable"`
	Data       map[string]string `yaml:"data"` // inline source (env-expanded)
}

type Resources struct {
	CPU struct {
		DefaultRequest string `yaml:"default_request"`
		DefaultLimit   string `yaml:"default_limit"`
	} `yaml:"cpu"`
	Memory struct {
		DefaultRequest string `yaml:"default_request"`
		DefaultLimit   string `yaml:"default_limit"`
	} `yaml:"memory"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type Auth struct {
	APIKeys          string `yaml:"api_keys"`
	Provider         string `yaml:"provider"`
	Token            string `yaml:"token"`
	AllowRemoteAdmin bool   `yaml:"allow_remote_admin"`
}

type SecretEncryption struct {
	Enabled bool      `yaml:"enabled"`
	KEK     KEKConfig `yaml:"kek"`
}

type KEKConfig struct {
	Source     string `yaml:"source"`
	File       string `yaml:"file"`
	Passphrase struct {
		Enabled bool   `yaml:"enabled"`
		Env     string `yaml:"env"`
	} `yaml:"passphrase"`
}

type Plugins struct {
	Dir     string   `yaml:"dir"`
	Enabled []string `yaml:"enabled"`
}

// Networking holds the networking-layer settings (RUNE-040..067).
//
// runed currently reads these via viper.GetString("networking.*")
// directly in cmd/runed/main.go rather than through the Config
// unmarshal path, but the fields are declared here so that the
// runefile schema-of-record stays complete (and so the lint-side
// parity test stays bidirectional). If/when the runed boot path
// migrates to reading these off Config, no further struct work is
// needed.
type Networking struct {
	ClusterCIDR string `yaml:"cluster_cidr"`
	DevMode     bool   `yaml:"dev_mode"`
}

// Telemetry holds the metrics endpoint configuration. See note on
// Networking above for the viper-direct-vs-unmarshal status.
type Telemetry struct {
	MetricsAddr string `yaml:"metrics_addr"`
}

// Node holds per-node placement metadata (edge / worker / leader).
// See note on Networking above.
type Node struct {
	Role string `yaml:"role"`
}

// Ingress holds the bind addresses for the edge ingress (RUNE-067).
// See note on Networking above.
type Ingress struct {
	HTTPAddr  string `yaml:"http_addr"`
	HTTPSAddr string `yaml:"https_addr"`
}

// ACME holds the Let's Encrypt directory + contact email used by the
// edge ingress (RUNE-067). See note on Networking above.
type ACME struct {
	Directory string `yaml:"directory"`
	Email     string `yaml:"email"`
}

// Storage holds operator-facing configuration for the storage
// subsystem. The runefile schema is:
//
//	storage:
//	  defaultStorageClass: local       # name of the cluster-default class
//	                                    # ("" disables the cluster default;
//	                                    # missing storageClassName then errors)
//	  preserveOnDelete: false          # when true, the local driver demotes
//	                                    # reclaimPolicy:delete to retain
//	  allowCreateMissing: false        # plumbing only; enforced by drivers
//	  drivers:
//	    local:
//	      localVolumeRoot: /var/lib/rune/volumes
//	    local-host:
//	      hostPathAllowlist: ["/srv/rune"]
//
// Keys under `drivers` are driver names registered with the storage
// driver registry. Values are passed verbatim to Driver constructors
// via OrchestratorOptions.StorageDriverConfigs; missing entries fall
// back to the driver's own defaults.
type Storage struct {
	// DefaultStorageClass selects which StorageClass is marked
	// Default:true at orchestrator boot. *string so the empty-string
	// case ("no cluster default — error on missing storageClassName")
	// is distinguishable from "unset → keep built-in default".
	DefaultStorageClass *string `yaml:"defaultStorageClass,omitempty"`

	// PreserveOnDelete, when true, converts ReclaimPolicy:delete to
	// retain for volumes provisioned by the in-tree "local" driver.
	// Useful for single-node dev clusters where accidental rm -rf is
	// unrecoverable.
	PreserveOnDelete bool `yaml:"preserveOnDelete,omitempty"`

	// AllowCreateMissing, when true, lets drivers auto-create missing
	// host paths instead of failing validation. Plumbed through to the
	// driver layer; per-driver enforcement is a separate slice.
	AllowCreateMissing bool `yaml:"allowCreateMissing,omitempty"`

	Drivers map[string]map[string]any `yaml:"drivers,omitempty"`
}

type Config struct {
	Server    Server    `yaml:"server"`
	UI        UI        `yaml:"ui"`
	DataDir   string    `yaml:"data_dir"`
	Client    Client    `yaml:"client"`
	Docker    Docker    `yaml:"docker"`
	Namespace string    `yaml:"namespace"`
	Auth      Auth      `yaml:"auth"`
	Resources Resources `yaml:"resources"`
	Log       Log       `yaml:"log"`
	Secret    struct {
		Encryption SecretEncryption `yaml:"encryption"`
		Limits     store.Limits     `yaml:"limits"`
	} `yaml:"secret"`
	ConfigResource struct {
		Limits store.Limits `yaml:"limits"`
	} `yaml:"config"`
	Plugins    Plugins    `yaml:"plugins"`
	Networking Networking `yaml:"networking"`
	Telemetry  Telemetry  `yaml:"telemetry"`
	Node       Node       `yaml:"node"`
	Ingress    Ingress    `yaml:"ingress"`
	ACME       ACME       `yaml:"acme"`
	Storage    Storage    `yaml:"storage"`

	// FailedInstanceRetention controls how long failed-but-preserved
	// instance containers stick around for postmortem (logs, exec --debug)
	// before the reconciler evicts them. See FailedInstanceRetention for
	// individual knobs and defaults.
	FailedInstanceRetention FailedInstanceRetention `yaml:"failed_instance_retention"`
}

// FailedInstanceRetention configures the reconciler's failed-instance GC.
// When an instance enters the Failed state, the runner stops (but does not
// remove) its container so operators can inspect it via `rune logs` and
// `rune exec --debug`. This struct bounds the resulting disk and container-
// slot growth.
type FailedInstanceRetention struct {
	// PerServiceCap is the maximum number of Failed instances to retain
	// per service before the oldest is evicted. 0 disables the cap.
	PerServiceCap int `yaml:"per_service_cap"`

	// TTL is the maximum age of a Failed instance before it is evicted,
	// regardless of cap. 0 disables TTL (only the cap applies).
	TTL time.Duration `yaml:"ttl"`

	// SnapshotLogBytes is the upper bound on the per-instance log
	// snapshot we capture into Instance.LastLogs at eviction time, so
	// `rune logs --previous` still has output to show after the
	// container is gone. 0 disables snapshotting.
	SnapshotLogBytes int `yaml:"snapshot_log_bytes"`
}

func Default() *Config {
	return &Config{
		Server:    Server{GRPCAddr: fmt.Sprintf(":%d", DefaultGRPCPort), HTTPAddr: fmt.Sprintf(":%d", DefaultHTTPPort)},
		UI:        UI{Enabled: true, Path: "/ui", HandoffEnabled: true, HandoffTTL: 60 * time.Second, RequireTLS: true},
		DataDir:   defaultDataDir(),
		Client:    Client{Timeout: 30 * time.Second, Retries: 3},
		Docker:    Docker{FallbackAPIVersion: "1.43", NegotiationTimeoutSeconds: 3},
		Namespace: "default",
		Log:       Log{Level: "info", Format: "text"},
		Secret: struct {
			Encryption SecretEncryption `yaml:"encryption"`
			Limits     store.Limits     `yaml:"limits"`
		}{
			// Default to file-based KEK with path derived from DataDir at runtime
			// so we can auto-generate on first run without root.
			Encryption: SecretEncryption{Enabled: true, KEK: KEKConfig{Source: "file", File: ""}},
			Limits:     store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
		},
		ConfigResource: struct {
			Limits store.Limits `yaml:"limits"`
		}{Limits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256}},
		FailedInstanceRetention: FailedInstanceRetention{
			PerServiceCap:    3,
			TTL:              1 * time.Hour,
			SnapshotLogBytes: 200_000,
		},
	}
}

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return "./data"
	}
	// prefer /var/lib/rune if exists and writable
	if st, err := os.Stat("/var/lib"); err == nil && st.IsDir() {
		return "/var/lib/rune"
	}
	return filepath.Join(home, ".rune")
}

func (c *Config) KEKOptions() *crypto.KEKOptions {
	return &crypto.KEKOptions{
		Source:   crypto.KEKSource(c.Secret.Encryption.KEK.Source),
		FilePath: c.Secret.Encryption.KEK.File,
		EnvVar:   "RUNE_MASTER_KEY",
		// Generate on first run if using file source and the file is missing,
		// or when Source is explicitly set to "generated".
		GenerateIfMissing: c.Secret.Encryption.KEK.Source == "generated" ||
			(c.Secret.Encryption.KEK.Source == "file" && c.Secret.Encryption.KEK.File != ""),
	}
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if path == "" {
		v.SetConfigName("runefile")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")          // Local development override
		v.AddConfigPath("/etc/rune/") // System-wide production config
	}
	cfg := Default()
	if err := v.ReadInConfig(); err == nil {
		if err := v.Unmarshal(cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}
