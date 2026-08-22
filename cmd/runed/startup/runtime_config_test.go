package startup

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

// TestInitRuntimeConfig_TOMLRoundTrip verifies that runefile.toml is
// auto-discovered and parsed with the same semantics as runefile.yaml.
// The networking-layer keys are exercised because they were the
// primary motivation for adding TOML support (RUNE-040..067).
func TestInitRuntimeConfig_TOMLRoundTrip(t *testing.T) {
	f := resetRuntimeConfigState(t)

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "runefile.toml")
	body := `data_dir = "` + dir + `"

[server]
grpc_address = ":17863"
http_address = ":17861"

[log]
level = "warn"

[networking]
cluster_cidr = "10.42.0.0/16"
dev_mode     = true

[telemetry]
metrics_addr = "127.0.0.1:29100"

[node]
role = "edge,worker"

[ingress]
http_addr  = ":18080"
https_addr = ":18443"

[acme]
directory = "https://pebble.test/dir"
email     = "ops@example.test"
`
	if err := os.WriteFile(tomlPath, []byte(body), 0600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	f.ConfigFile = tomlPath

	resolveRuntimeConfig(f)

	if got, want := f.GRPCAddr, ":17863"; got != want {
		t.Errorf("grpcAddr = %q, want %q", got, want)
	}
	if got, want := f.HTTPAddr, ":17861"; got != want {
		t.Errorf("httpAddr = %q, want %q", got, want)
	}
	if got, want := f.LogLevel, "warn"; got != want {
		t.Errorf("logLevel = %q, want %q", got, want)
	}
	if got, want := f.ClusterCIDR, "10.42.0.0/16"; got != want {
		t.Errorf("clusterCIDR = %q, want %q", got, want)
	}
	if !f.DevMode {
		t.Errorf("devMode = false, want true")
	}
	if got, want := f.MetricsAddr, "127.0.0.1:29100"; got != want {
		t.Errorf("metricsAddr = %q, want %q", got, want)
	}
	if got, want := f.NodeRole, "edge,worker"; got != want {
		t.Errorf("nodeRole = %q, want %q", got, want)
	}
	if got, want := f.IngressHTTP, ":18080"; got != want {
		t.Errorf("ingressHTTP = %q, want %q", got, want)
	}
	if got, want := f.IngressHTTPS, ":18443"; got != want {
		t.Errorf("ingressHTTPS = %q, want %q", got, want)
	}
	if got, want := f.ACMEDirectory, "https://pebble.test/dir"; got != want {
		t.Errorf("acmeDirectory = %q, want %q", got, want)
	}
	if got, want := f.ACMEEmail, "ops@example.test"; got != want {
		t.Errorf("acmeEmail = %q, want %q", got, want)
	}
}

// TestInitRuntimeConfig_EnvOverridesTOML verifies that an environment
// variable overrides a value loaded from a TOML runefile.
func TestInitRuntimeConfig_EnvOverridesTOML(t *testing.T) {
	f := resetRuntimeConfigState(t)

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "runefile.toml")
	body := `[networking]
cluster_cidr = "10.42.0.0/16"

[acme]
email = "from-file@example.test"
`
	if err := os.WriteFile(tomlPath, []byte(body), 0600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	f.ConfigFile = tomlPath
	t.Setenv("RUNE_NETWORKING_CLUSTER_CIDR", "10.99.0.0/16")
	t.Setenv("RUNE_ACME_EMAIL", "from-env@example.test")

	resolveRuntimeConfig(f)

	if got, want := f.ClusterCIDR, "10.99.0.0/16"; got != want {
		t.Errorf("clusterCIDR = %q, want %q (env should win)", got, want)
	}
	if got, want := f.ACMEEmail, "from-env@example.test"; got != want {
		t.Errorf("acmeEmail = %q, want %q (env should win)", got, want)
	}
}

// TestInitRuntimeConfig_FlagOverridesEnvAndFile verifies that an
// explicit command-line flag wins over both env vars and the file.
func TestInitRuntimeConfig_FlagOverridesEnvAndFile(t *testing.T) {
	f := resetRuntimeConfigState(t)

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "runefile.toml")
	body := `[ingress]
http_addr = ":18080"
`
	if err := os.WriteFile(tomlPath, []byte(body), 0600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	f.ConfigFile = tomlPath
	t.Setenv("RUNE_INGRESS_HTTP_ADDR", ":28080")

	// Simulate an explicit --ingress-http-addr=:38080 invocation by
	// directly mutating the flag's value AND marking it as set.
	if err := flag.Set("ingress-http-addr", ":38080"); err != nil {
		t.Fatalf("flag set: %v", err)
	}

	resolveRuntimeConfig(f)

	if got, want := f.IngressHTTP, ":38080"; got != want {
		t.Errorf("ingressHTTP = %q, want %q (flag should win over env+file)", got, want)
	}
}

// resetRuntimeConfigState resets the package-level state resolveRuntimeConfig
// mutates so tests can run in any order. Without this the flag.Visit gate
// persists across tests and pollutes precedence.
//
// It returns a fresh Flags registered on the new flag set — DefineFlags is the
// single declaration of the flag surface, so this helper can no longer drift
// from it the way a hand-mirrored var block did.
func resetRuntimeConfigState(t *testing.T) *Flags {
	t.Helper()
	viper.Reset()
	// Reset the flag set so flag.Visit returns no flags as "set" by
	// default. Tests that need a flag-set state call flag.Set after
	// this hook.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	return DefineFlags(flag.CommandLine)
}
