package main

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
	resetRuntimeConfigState(t)

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
	*configFile = tomlPath

	initRuntimeConfig()

	if got, want := *grpcAddr, ":17863"; got != want {
		t.Errorf("grpcAddr = %q, want %q", got, want)
	}
	if got, want := *httpAddr, ":17861"; got != want {
		t.Errorf("httpAddr = %q, want %q", got, want)
	}
	if got, want := *logLevel, "warn"; got != want {
		t.Errorf("logLevel = %q, want %q", got, want)
	}
	if got, want := *clusterCIDR, "10.42.0.0/16"; got != want {
		t.Errorf("clusterCIDR = %q, want %q", got, want)
	}
	if !*devMode {
		t.Errorf("devMode = false, want true")
	}
	if got, want := *metricsAddr, "127.0.0.1:29100"; got != want {
		t.Errorf("metricsAddr = %q, want %q", got, want)
	}
	if got, want := *nodeRole, "edge,worker"; got != want {
		t.Errorf("nodeRole = %q, want %q", got, want)
	}
	if got, want := *ingressHTTP, ":18080"; got != want {
		t.Errorf("ingressHTTP = %q, want %q", got, want)
	}
	if got, want := *ingressHTTPS, ":18443"; got != want {
		t.Errorf("ingressHTTPS = %q, want %q", got, want)
	}
	if got, want := *acmeDirectory, "https://pebble.test/dir"; got != want {
		t.Errorf("acmeDirectory = %q, want %q", got, want)
	}
	if got, want := *acmeEmail, "ops@example.test"; got != want {
		t.Errorf("acmeEmail = %q, want %q", got, want)
	}
}

// TestInitRuntimeConfig_EnvOverridesTOML verifies that an environment
// variable overrides a value loaded from a TOML runefile.
func TestInitRuntimeConfig_EnvOverridesTOML(t *testing.T) {
	resetRuntimeConfigState(t)

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
	*configFile = tomlPath
	t.Setenv("RUNE_NETWORKING_CLUSTER_CIDR", "10.99.0.0/16")
	t.Setenv("RUNE_ACME_EMAIL", "from-env@example.test")

	initRuntimeConfig()

	if got, want := *clusterCIDR, "10.99.0.0/16"; got != want {
		t.Errorf("clusterCIDR = %q, want %q (env should win)", got, want)
	}
	if got, want := *acmeEmail, "from-env@example.test"; got != want {
		t.Errorf("acmeEmail = %q, want %q (env should win)", got, want)
	}
}

// TestInitRuntimeConfig_FlagOverridesEnvAndFile verifies that an
// explicit command-line flag wins over both env vars and the file.
func TestInitRuntimeConfig_FlagOverridesEnvAndFile(t *testing.T) {
	resetRuntimeConfigState(t)

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "runefile.toml")
	body := `[ingress]
http_addr = ":18080"
`
	if err := os.WriteFile(tomlPath, []byte(body), 0600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	*configFile = tomlPath
	t.Setenv("RUNE_INGRESS_HTTP_ADDR", ":28080")

	// Simulate an explicit --ingress-http-addr=:38080 invocation by
	// directly mutating the flag's value AND marking it as set.
	if err := flag.Set("ingress-http-addr", ":38080"); err != nil {
		t.Fatalf("flag set: %v", err)
	}

	initRuntimeConfig()

	if got, want := *ingressHTTP, ":38080"; got != want {
		t.Errorf("ingressHTTP = %q, want %q (flag should win over env+file)", got, want)
	}
}

// resetRuntimeConfigState resets the package-level state mutated by
// initRuntimeConfig so tests can run in any order. Without this the
// flag.Visit gate persists across tests and pollutes precedence.
func resetRuntimeConfigState(t *testing.T) {
	t.Helper()
	viper.Reset()
	// Reset the flag set so flag.Visit returns no flags as "set" by
	// default. Tests that need a flag-set state call flag.Set after
	// this hook.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	// Re-declare the flags this package owns, mirroring main.go's
	// var-block defaults. Pointers are swapped to the new flag set.
	configFile = flag.String("config", "", "")
	grpcAddr = flag.String("grpc-addr", ":7863", "")
	httpAddr = flag.String("http-addr", ":7861", "")
	dataDir = flag.String("data-dir", "", "")
	logLevel = flag.String("log-level", "info", "")
	debugLogLevel = flag.Bool("debug", false, "")
	logFormat = flag.String("log-format", "text", "")
	prettyLogs = flag.Bool("pretty", false, "")
	devMode = flag.Bool("dev-mode", false, "")
	clusterCIDR = flag.String("cluster-cidr", "10.96.0.0/16", "")
	metricsAddr = flag.String("metrics-addr", "127.0.0.1:9100", "")
	nodeRole = flag.String("node-role", "", "")
	ingressHTTP = flag.String("ingress-http-addr", "", "")
	ingressHTTPS = flag.String("ingress-https-addr", "", "")
	acmeDirectory = flag.String("acme-directory", "", "")
	acmeEmail = flag.String("acme-email", "", "")
	showHelp = flag.Bool("help", false, "")
	showVer = flag.Bool("version", false, "")
}
