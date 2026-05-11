package types_test

// Tests covering the runefile-schema work in RUNE-111 (registries
// subtree must lint clean) and RUNE-113 (TOML files must be detected
// + parsed, not silently treated as invalid YAML).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// productionRunefileYAML mirrors the runefile rendered by
// terraform-digitalocean-rune's `with-bootstrap` example: it exercises
// every top-level section that runed accepts plus the docker.registries
// subtree with auth.fromSecret + auth.data, which is the exact shape
// that used to trip every lint run with false-positive errors before
// RUNE-111.
const productionRunefileYAML = `
server:
  grpc_address: ":7863"
  http_address: ":7861"
data_dir: /var/lib/rune
log:
  level: info
  format: json
auth:
  api_keys: ""
  allow_remote_admin: false
namespace: default
networking:
  cluster_cidr: 10.96.0.0/16
  dev_mode: false
telemetry:
  metrics_addr: 127.0.0.1:9100
node:
  role: leader
ingress:
  http_addr: ":80"
  https_addr: ":443"
acme:
  directory: https://acme-v02.api.letsencrypt.org/directory
  email: ops@example.com
secret:
  encryption:
    enabled: true
    kek:
      source: file
      file: /var/lib/rune/kek.bin
  limits:
    max_object_bytes: 1048576
    max_key_name_length: 256
config:
  limits:
    max_object_bytes: 1048576
    max_key_name_length: 256
docker:
  api_version: "1.43"
  fallback_api_version: "1.43"
  negotiation_timeout_seconds: 3
  registries:
    - name: ghcr
      registry: ghcr.io
      auth:
        fromSecret: ghcr-pull
        bootstrap: true
        manage: update
        immutable: false
        data:
          username: bot
          password: ${GHCR_PAT}
`

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestParseRuneFile_ProductionYAML asserts the canonical TF-generated
// runefile parses without any "unknown field" complaints. Pre-RUNE-111
// the parser would silently drop registries / networking / etc. and the
// linter would emit a wall of false positives downstream.
func TestParseRuneFile_ProductionYAML(t *testing.T) {
	p := writeTemp(t, "runefile.yaml", productionRunefileYAML)
	rf, err := types.ParseRuneFile(p)
	if err != nil {
		t.Fatalf("ParseRuneFile: %v", err)
	}
	if rf.Docker == nil || len(rf.Docker.Registries) != 1 {
		t.Fatalf("expected 1 docker registry, got %#v", rf.Docker)
	}
	r := rf.Docker.Registries[0]
	if r.Name != "ghcr" || r.Registry != "ghcr.io" {
		t.Errorf("registry name/host wrong: %+v", r)
	}
	if r.Auth.FromSecret == nil || r.Auth.Bootstrap != true || r.Auth.Manage != "update" {
		t.Errorf("registry auth fields not parsed: %+v", r.Auth)
	}
	if r.Auth.Data["username"] != "bot" || !strings.Contains(r.Auth.Data["password"], "GHCR_PAT") {
		t.Errorf("registry auth.data not parsed: %+v", r.Auth.Data)
	}
	if rf.Networking == nil || rf.Networking.ClusterCIDR != "10.96.0.0/16" {
		t.Errorf("networking section not parsed: %+v", rf.Networking)
	}
	if rf.ACME == nil || rf.ACME.Email != "ops@example.com" {
		t.Errorf("acme section not parsed: %+v", rf.ACME)
	}
	if rf.Config == nil || rf.Config.Limits == nil || rf.Config.Limits.MaxObjectBytes != 1048576 {
		t.Errorf("top-level config.limits not parsed: %+v", rf.Config)
	}
	if rf.Secret == nil || rf.Secret.Limits == nil || rf.Secret.Limits.MaxKeyNameLength != 256 {
		t.Errorf("secret.limits not parsed: %+v", rf.Secret)
	}
}

// productionRunefileTOML is the TOML projection of the YAML fixture
// above — same values, same shape, but in the format that
// install-server.sh and the terraform module actually write to disk.
const productionRunefileTOML = `
data_dir = "/var/lib/rune"
namespace = "default"

[server]
grpc_address = ":7863"
http_address = ":7861"

[log]
level = "info"
format = "json"

[auth]
api_keys = ""
allow_remote_admin = false

[networking]
cluster_cidr = "10.96.0.0/16"
dev_mode = false

[telemetry]
metrics_addr = "127.0.0.1:9100"

[node]
role = "leader"

[ingress]
http_addr = ":80"
https_addr = ":443"

[acme]
directory = "https://acme-v02.api.letsencrypt.org/directory"
email = "ops@example.com"

[secret.encryption]
enabled = true

[secret.encryption.kek]
source = "file"
file = "/var/lib/rune/kek.bin"

[secret.limits]
max_object_bytes = 1048576
max_key_name_length = 256

[config.limits]
max_object_bytes = 1048576
max_key_name_length = 256

[docker]
api_version = "1.43"
fallback_api_version = "1.43"
negotiation_timeout_seconds = 3

[[docker.registries]]
name = "ghcr"
registry = "ghcr.io"

[docker.registries.auth]
fromSecret = "ghcr-pull"
bootstrap = true
manage = "update"
immutable = false

[docker.registries.auth.data]
username = "bot"
password = "${GHCR_PAT}"
`

// TestIsRuneConfigFile_TOML asserts that a runefile.toml is recognised
// by the lint-detector. Pre-RUNE-113 IsRuneConfigFile called
// yaml.Unmarshal on the raw bytes, which on a TOML file either errors
// or returns an empty document — both of which made `rune lint` skip
// the file silently (or treat it as a castfile).
func TestIsRuneConfigFile_TOML(t *testing.T) {
	p := writeTemp(t, "runefile.toml", productionRunefileTOML)
	ok, err := types.IsRuneConfigFile(p)
	if err != nil {
		t.Fatalf("IsRuneConfigFile(toml): %v", err)
	}
	if !ok {
		t.Fatalf("IsRuneConfigFile returned false for a valid TOML runefile")
	}
}

// TestParseRuneFile_TOML asserts the TOML decode + transcode + struct
// unmarshal pipeline produces the same RuneFile as the YAML path.
func TestParseRuneFile_TOML(t *testing.T) {
	p := writeTemp(t, "runefile.toml", productionRunefileTOML)
	rf, err := types.ParseRuneFile(p)
	if err != nil {
		t.Fatalf("ParseRuneFile(toml): %v", err)
	}
	if rf.Docker == nil || len(rf.Docker.Registries) != 1 {
		t.Fatalf("expected 1 docker registry from TOML, got %#v", rf.Docker)
	}
	r := rf.Docker.Registries[0]
	if r.Name != "ghcr" || r.Auth.Manage != "update" || !r.Auth.Bootstrap {
		t.Errorf("registry not parsed from TOML: %+v / %+v", r, r.Auth)
	}
	if r.Auth.FromSecret != "ghcr-pull" {
		t.Errorf("auth.fromSecret not parsed from TOML: %#v", r.Auth.FromSecret)
	}
	if rf.Networking == nil || rf.Networking.ClusterCIDR != "10.96.0.0/16" {
		t.Errorf("networking not parsed from TOML: %+v", rf.Networking)
	}
}

// TestIsRuneConfigFile_YAMLUnchanged is a regression guard: existing
// YAML detection must still work after the TOML routing was added.
func TestIsRuneConfigFile_YAMLUnchanged(t *testing.T) {
	p := writeTemp(t, "runefile.yaml", productionRunefileYAML)
	ok, err := types.IsRuneConfigFile(p)
	if err != nil {
		t.Fatalf("IsRuneConfigFile(yaml): %v", err)
	}
	if !ok {
		t.Fatalf("IsRuneConfigFile returned false for a valid YAML runefile")
	}
}
