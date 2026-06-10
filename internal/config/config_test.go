package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The runefile schema-of-record is the `yaml` struct tag, but viper's default
// Unmarshal decodes via `mapstructure` tags, falling back to case-insensitive
// FIELD-NAME matching. Single-word keys (enabled, backend) matched by luck;
// every multi-word snake_case key (handoff_ttl, retention_days, data_dir, ...)
// was silently dropped, leaving the default in place with no error. This bit
// us in production debugging (ui.handoff_ttl overrides ignored). Load must
// decode against the yaml tags.
func TestLoad_DecodesYAMLTagKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runefile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
data_dir: /custom/data
namespace: staging
server:
  grpc_address: ":9999"
  http_address: ":9998"
  tls:
    cert_file: /etc/rune/tls.crt
    key_file: /etc/rune/tls.key
ui:
  enabled: true
  handoff_ttl: 5m
  require_tls: false
docker:
  log_max_file: 7
auth:
  allow_remote_admin: true
  session_access_ttl: 90m
networking:
  cluster_cidr: 10.100.0.0/16
telemetry:
  metrics_addr: 127.0.0.1:9200
ingress:
  http_addr: ":8081"
  https_addr: ":8444"
observability:
  enabled: true
  backend: loki
  retention_days: 14
  objectStore:
    enabled: true
    endpoint: http://loki:3100
    bucket: rune-logs
failed_instance_retention:
  per_service_cap: 9
  ttl: 36h
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Multi-word snake_case keys — every one of these was silently dropped
	// before the yaml-tag decode fix.
	assert.Equal(t, "/custom/data", cfg.DataDir, "data_dir")
	assert.Equal(t, ":9999", cfg.Server.GRPCAddr, "server.grpc_address")
	assert.Equal(t, ":9998", cfg.Server.HTTPAddr, "server.http_address")
	assert.Equal(t, "/etc/rune/tls.crt", cfg.Server.TLS.CertFile, "server.tls.cert_file")
	assert.Equal(t, "/etc/rune/tls.key", cfg.Server.TLS.KeyFile, "server.tls.key_file")
	assert.Equal(t, 5*time.Minute, cfg.UI.HandoffTTL, "ui.handoff_ttl (duration)")
	assert.False(t, cfg.UI.RequireTLS, "ui.require_tls (non-default bool)")
	assert.Equal(t, 7, cfg.Docker.LogMaxFile, "docker.log_max_file")
	assert.True(t, cfg.Auth.AllowRemoteAdmin, "auth.allow_remote_admin")
	assert.Equal(t, 90*time.Minute, cfg.Auth.SessionAccessTTL, "auth.session_access_ttl (duration)")
	assert.Equal(t, "10.100.0.0/16", cfg.Networking.ClusterCIDR, "networking.cluster_cidr")
	assert.Equal(t, "127.0.0.1:9200", cfg.Telemetry.MetricsAddr, "telemetry.metrics_addr")
	assert.Equal(t, ":8081", cfg.Ingress.HTTPAddr, "ingress.http_addr")
	assert.Equal(t, ":8444", cfg.Ingress.HTTPSAddr, "ingress.https_addr")
	assert.Equal(t, 14, cfg.Observability.RetentionDays, "observability.retention_days")
	assert.Equal(t, 9, cfg.FailedInstanceRetention.PerServiceCap,
		"failed_instance_retention.per_service_cap")
	assert.Equal(t, 36*time.Hour, cfg.FailedInstanceRetention.TTL,
		"failed_instance_retention.ttl (duration)")

	// Single-word and camelCase keys that worked before must keep working.
	assert.Equal(t, "staging", cfg.Namespace, "namespace")
	assert.True(t, cfg.Observability.Enabled, "observability.enabled")
	assert.Equal(t, "loki", cfg.Observability.Backend, "observability.backend")
	assert.True(t, cfg.Observability.ObjectStore.Enabled, "objectStore.enabled")
	assert.Equal(t, "http://loki:3100", cfg.Observability.ObjectStore.Endpoint, "objectStore.endpoint")
	assert.Equal(t, "rune-logs", cfg.Observability.ObjectStore.Bucket, "objectStore.bucket")
}

// Keys absent from the runefile keep their Default() values.
func TestLoad_UnsetKeysKeepDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runefile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
ui:
  handoff_ttl: 2m
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, cfg.UI.HandoffTTL)
	// Untouched defaults survive a partial file.
	def := Default()
	assert.Equal(t, def.UI.Path, cfg.UI.Path)
	assert.Equal(t, def.Server.GRPCAddr, cfg.Server.GRPCAddr)
	assert.Equal(t, def.Secret.Limits.MaxObjectBytes, cfg.Secret.Limits.MaxObjectBytes)
}

// A missing config file is not an error — Load returns pure defaults.
func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	def := Default()
	assert.Equal(t, def.UI.HandoffTTL, cfg.UI.HandoffTTL)
	assert.Equal(t, def.Namespace, cfg.Namespace)
}
