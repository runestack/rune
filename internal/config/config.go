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

type Config struct {
	Server    Server    `yaml:"server"`
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
	Plugins Plugins `yaml:"plugins"`
}

func Default() *Config {
	return &Config{
		Server:    Server{GRPCAddr: fmt.Sprintf(":%d", DefaultGRPCPort), HTTPAddr: fmt.Sprintf(":%d", DefaultHTTPPort)},
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
