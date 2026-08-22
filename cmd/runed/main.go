package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/log"
	acmesvc "github.com/runestack/rune/pkg/networking/acme"
	"github.com/runestack/rune/pkg/networking/ingress"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/version"
	"github.com/spf13/viper"

	// Storage drivers — each blank-import registers one or more driver
	// names with pkg/storage/driver.Registry at init() time. Adding a new
	// driver is one more line here.
	_ "github.com/runestack/rune/pkg/storage/driver/awsebs"
	_ "github.com/runestack/rune/pkg/storage/driver/dovolume"
	_ "github.com/runestack/rune/pkg/storage/driver/gcepd"
	_ "github.com/runestack/rune/pkg/storage/driver/hcloudvolume"
	_ "github.com/runestack/rune/pkg/storage/driver/local"
)

// acmeCertStoreWithReload wraps a CertStore so that successful Set
// calls trigger a refresh of the ingress CertLoader cache. Without
// this, newly-issued certificates would not be served until a process
// restart.
type acmeCertStoreWithReload struct {
	store  acmesvc.CertStore
	loader *ingress.CertLoader
}

func (w acmeCertStoreWithReload) Set(ctx context.Context, host string, cert, key []byte) error {
	if err := w.store.Set(ctx, host, cert, key); err != nil {
		return err
	}
	return w.loader.Reload(ctx, host)
}

func (w acmeCertStoreWithReload) Get(ctx context.Context, host string) ([]byte, []byte, error) {
	return w.store.Get(ctx, host)
}

func (w acmeCertStoreWithReload) Delete(ctx context.Context, host string) error {
	w.loader.Forget(host)
	return w.store.Delete(ctx, host)
}

// notReadyMountResolver is the pre-agent MountResolver the orchestrator
// is seeded with at startup. Every lookup returns ("", false) so the
// instance controller treats every volume as "not yet mounted" and
// retries on the next reconcile tick — the documented transient
// condition. When the agent volumes subsystem comes up it calls
// SetMountResolver again with its real implementation, replacing this
// stub. Without it, the few seconds between apiServer.Start and the
// agent's Subsystem registration race: the controller sees a nil
// resolver, falls through to using Volume.Handle as the bind source,
// and cloud-driver volumes (where Handle is a UUID, not a path) fail
// with "invalid mount path". RUNE-BUG-DOVOLUME-ATTACH-NOOP-AND-MOUNT-PERMS.
type notReadyMountResolver struct{}

func (notReadyMountResolver) MountTargetFor(string) (string, bool) {
	return "", false
}

// acmeNoopStatus discards status updates. Until the service-watch
// wiring lands, there is no Service object to mutate; the orchestrator
// still records state in its in-memory tracker which is enough for
// observability via /metrics.
type acmeNoopStatus struct {
	logger log.Logger
}

func (s acmeNoopStatus) UpdateIngressCert(_ context.Context, ns, name string, st types.IngressCertStatus) error {
	s.logger.Debug("ingress cert status",
		log.Str("namespace", ns),
		log.Str("service", name),
		log.Str("host", st.Host),
		log.Str("state", string(st.State)),
		log.Str("error", st.LastError))
	return nil
}

var (
	configFile    = flag.String("config", "", "Path to runefile.yaml (server configuration)")
	grpcAddr      = flag.String("grpc-addr", ":7863", "gRPC server address")
	httpAddr      = flag.String("http-addr", ":7861", "HTTP server address")
	dataDir       = flag.String("data-dir", "", "Data directory (if not specified, uses OS-specific application data directory)")
	logLevel      = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	debugLogLevel = flag.Bool("debug", false, "Enable debug mode (shorthand for --log-level=debug)")
	logFormat     = flag.String("log-format", "text", "Log format (text, json)")
	prettyLogs    = flag.Bool("pretty", false, "Enable pretty text log format (shorthand for --log-format=text)")
	devMode       = flag.Bool("dev-mode", false, "Run in dev mode: skip nftables, bind ingress on user ports, embedded DNS resolves .rune to 127.0.0.1 (laptop development)")
	clusterCIDR   = flag.String("cluster-cidr", "10.96.0.0/16", "Cluster service CIDR for VIP allocation (RFC1918 or 100.64/10)")
	metricsAddr   = flag.String("metrics-addr", "127.0.0.1:9100", "Address for the Prometheus /metrics endpoint (empty disables). Exposes metrics from all subsystems (orchestrator, runners, networking, agent, DNS).")
	nodeRole      = flag.String("node-role", "", "Comma-separated node roles. 'edge' enables the ingress controller and ACME orchestrator on this node.")
	ingressHTTP   = flag.String("ingress-http-addr", "", "Bind address for the ingress HTTP listener. Defaults to :80 in production, :8080 in dev mode. Used only when node-role contains 'edge'.")
	ingressHTTPS  = flag.String("ingress-https-addr", "", "Bind address for the ingress HTTPS listener. Defaults to :443 in production, :8443 in dev mode. Used only when node-role contains 'edge'. Empty disables TLS termination.")
	acmeDirectory = flag.String("acme-directory", "", "ACME directory URL. Empty defaults to Let's Encrypt production. Use a Pebble URL for integration tests.")
	acmeEmail     = flag.String("acme-email", "", "Contact email passed to the ACME provider on account registration.")
	uiEnabled     = flag.Bool("ui", true, "Serve the embedded web dashboard and gRPC-Web transcoder on the HTTP address (RUNE-200). Use --ui=false for headless installs.")
	uiPath        = flag.String("ui-path", "", "Mount path for the embedded dashboard (default /ui).")
	uiRequireTLS  = flag.Bool("ui-require-tls", true, "Refuse to serve the dashboard over plaintext on a non-loopback address; bind 127.0.0.1 only when TLS is off. Set false for dev or when TLS terminates at an ingress.")
	showHelp      = flag.Bool("help", false, "Show help")
	showVer       = flag.Bool("version", false, "Show version")
)

// getDefaultDataDir returns the default data directory based on the OS
func getDefaultDataDir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "./data"
	}

	// OS-specific paths
	switch {
	case os.Getenv("XDG_DATA_HOME") != "":
		// Linux with XDG
		return filepath.Join(os.Getenv("XDG_DATA_HOME"), "rune")
	case isDir("/var/lib"):
		// Linux/Unix system dir
		return "/var/lib/rune"
	case isDir(filepath.Join(homeDir, "Library")):
		// macOS
		return filepath.Join(homeDir, "Library", "Application Support", "Rune")
	case isDir(filepath.Join(homeDir, "AppData")):
		// Windows
		return filepath.Join(homeDir, "AppData", "Local", "Rune")
	default:
		// Fallback
		return filepath.Join(homeDir, ".rune")
	}
}

// isDir checks if a path exists and is a directory
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// findRunefile returns the first runefile.{toml,yaml,yml} found in
// the supplied search paths. The returned path is suitable for
// viper.SetConfigFile (extension drives parser selection). Returns
// an empty string if no runefile is found.
func findRunefile(searchPaths []string) string {
	for _, dir := range searchPaths {
		for _, ext := range []string{"toml", "yaml", "yml"} {
			candidate := filepath.Join(dir, "runefile."+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}

// resolveRunefilePath returns the runefile path runed should load.
// Precedence: --config flag → auto-discovery in cwd then /etc/rune.
// Returns an empty string if neither produced a match; callers can
// proceed with built-in defaults (e.g. for unit tests).
func resolveRunefilePath() string {
	if *configFile != "" {
		return *configFile
	}
	return findRunefile([]string{".", "/etc/rune"})
}

// initRuntimeConfig initializes runtime settings (viper defaults + config file + env + flags)
func initRuntimeConfig() {
	// Initialize viper
	v := viper.New()

	// 1. Set default values that will be used if nothing else is specified
	defaultDataDir := getDefaultDataDir()
	v.SetDefault("server.grpc_address", fmt.Sprintf(":%d", config.DefaultGRPCPort))
	v.SetDefault("server.http_address", fmt.Sprintf(":%d", config.DefaultHTTPPort))
	v.SetDefault("data_dir", defaultDataDir)
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "text")
	v.SetDefault("auth.api_keys", "")

	// Docker related defaults
	v.SetDefault("docker.fallback_api_version", "1.43")
	v.SetDefault("docker.negotiation_timeout_seconds", 3)

	// Networking layer defaults (RUNE-040..067). These mirror the
	// flag defaults so that a runefile can override them without
	// having to also pass the corresponding flag.
	v.SetDefault("networking.cluster_cidr", "10.96.0.0/16")
	v.SetDefault("networking.dev_mode", false)
	v.SetDefault("telemetry.metrics_addr", "127.0.0.1:9100")
	v.SetDefault("node.role", "")
	v.SetDefault("ingress.http_addr", "")
	v.SetDefault("ingress.https_addr", "")
	v.SetDefault("acme.directory", "")
	v.SetDefault("acme.email", "")

	// 2. Load the runefile. Both YAML and TOML are supported; viper picks
	// the parser from the file extension when SetConfigFile is used.
	// Search order: --config flag → cwd → /etc/rune (see resolveRunefilePath).
	resolvedRunefile := resolveRunefilePath()
	if resolvedRunefile != "" {
		v.SetConfigFile(resolvedRunefile)
		if err := v.ReadInConfig(); err != nil {
			fmt.Printf("Error reading config file %s: %s\n", resolvedRunefile, err)
		} else {
			fmt.Printf("Using config file: %s\n", v.ConfigFileUsed())
		}
	}

	// 3. Override with environment variables
	v.SetEnvPrefix("RUNE")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Explicitly bind key environment variables
	// This is the Kubernetes approach - explicitly declare the env vars we care about
	envVarMappings := map[string]string{
		// Server config
		"RUNE_SERVER_GRPC_ADDRESS": "server.grpc_address",
		"RUNE_SERVER_HTTP_ADDRESS": "server.http_address",
		"RUNE_DATA_DIR":            "data_dir",

		// Docker config - explicitly support the earlier direct env var
		"RUNE_DOCKER_API_VERSION":          "docker.api_version",
		"RUNE_DOCKER_FALLBACK_API_VERSION": "docker.fallback_api_version",
		"RUNE_DOCKER_NEGOTIATION_TIMEOUT":  "docker.negotiation_timeout_seconds",

		// Log config
		"RUNE_LOG_LEVEL":  "log.level",
		"RUNE_LOG_FORMAT": "log.format",

		// Auth config
		"RUNE_AUTH_API_KEYS":           "auth.api_keys",
		"RUNE_AUTH_ALLOW_REMOTE_ADMIN": "auth.allow_remote_admin",

		// Networking layer (RUNE-040..067)
		"RUNE_NETWORKING_CLUSTER_CIDR": "networking.cluster_cidr",
		"RUNE_NETWORKING_DEV_MODE":     "networking.dev_mode",
		"RUNE_TELEMETRY_METRICS_ADDR":  "telemetry.metrics_addr",
		"RUNE_NODE_ROLE":               "node.role",
		"RUNE_INGRESS_HTTP_ADDR":       "ingress.http_addr",
		"RUNE_INGRESS_HTTPS_ADDR":      "ingress.https_addr",
		"RUNE_ACME_DIRECTORY":          "acme.directory",
		"RUNE_ACME_EMAIL":              "acme.email",
	}

	// Explicitly bind environment variables to configuration keys
	for env, configKey := range envVarMappings {
		_ = v.BindEnv(configKey, env)
	}

	// 4. Track which parameters were explicitly set via command-line flags
	// These will override everything else
	cmdFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		cmdFlags[f.Name] = true
	})

	// 5. Apply values in order of precedence:
	// Command-line flags (already set) > env vars > config file > defaults (already set)

	// Only apply values from config/env if not explicitly set by command-line flags
	if !cmdFlags["grpc-addr"] {
		*grpcAddr = v.GetString("server.grpc_address")
	}

	if !cmdFlags["http-addr"] {
		*httpAddr = v.GetString("server.http_address")
	}

	if !cmdFlags["data-dir"] {
		dataDirFromConfig := v.GetString("data_dir")
		if dataDirFromConfig != "" {
			*dataDir = dataDirFromConfig
		} else {
			*dataDir = defaultDataDir
		}
	}

	if !cmdFlags["log-level"] {
		*logLevel = v.GetString("log.level")
	}

	if !cmdFlags["log-format"] {
		*logFormat = v.GetString("log.format")
	}

	if !cmdFlags["debug"] {
		*debugLogLevel = v.GetBool("debug")
	}

	if !cmdFlags["pretty"] {
		*prettyLogs = v.GetBool("pretty")
	}

	// Networking layer flag precedence (RUNE-040..067).
	if !cmdFlags["cluster-cidr"] {
		if s := v.GetString("networking.cluster_cidr"); s != "" {
			*clusterCIDR = s
		}
	}
	if !cmdFlags["dev-mode"] {
		*devMode = v.GetBool("networking.dev_mode")
	}
	if !cmdFlags["metrics-addr"] {
		// Empty string is a valid value (disables metrics) so use IsSet
		// to distinguish "explicitly set to empty" from "unset".
		if v.IsSet("telemetry.metrics_addr") {
			*metricsAddr = v.GetString("telemetry.metrics_addr")
		}
	}
	if !cmdFlags["node-role"] {
		*nodeRole = v.GetString("node.role")
	}
	// Dev mode is single-node by definition. Auto-promote the node to
	// 'edge' so the ingress controller and ACME orchestrator come up
	// without forcing developers to remember a second flag. An explicit
	// --node-role (including --node-role="" to opt out) is respected.
	if *devMode && !cmdFlags["node-role"] && *nodeRole == "" {
		*nodeRole = "edge"
	}
	if !cmdFlags["ingress-http-addr"] {
		*ingressHTTP = v.GetString("ingress.http_addr")
	}
	if !cmdFlags["ingress-https-addr"] {
		*ingressHTTPS = v.GetString("ingress.https_addr")
	}
	if !cmdFlags["acme-directory"] {
		*acmeDirectory = v.GetString("acme.directory")
	}
	if !cmdFlags["acme-email"] {
		*acmeEmail = v.GetString("acme.email")
	}

	// Dashboard UI flag precedence (RUNE-200). Defaults match config.Default()
	// (enabled, require_tls), so the flag default holds unless the runefile or
	// an explicit flag overrides it.
	if !cmdFlags["ui"] && v.IsSet("ui.enabled") {
		*uiEnabled = v.GetBool("ui.enabled")
	}
	if !cmdFlags["ui-path"] && v.IsSet("ui.path") {
		*uiPath = v.GetString("ui.path")
	}
	if !cmdFlags["ui-require-tls"] && v.IsSet("ui.require_tls") {
		*uiRequireTLS = v.GetBool("ui.require_tls")
	}

	// Final validation and defaults for required parameters
	if *dataDir == "" {
		*dataDir = defaultDataDir
	}
}

func main() {
	// Helper subcommands (e.g. `runed print-unit`) short-circuit the
	// daemon flag parser so they can own their own flag set without
	// polluting the daemon's `var (...)` block. See print_unit.go.
	if handled, code := dispatchSubcommand(); handled {
		os.Exit(code)
	}

	// Parse flags
	flag.Parse()

	// Show help if requested
	if *showHelp {
		flag.Usage()
		return
	}

	// Show version if requested
	if *showVer {
		fmt.Println(version.Info())
		return
	}

	// Startup phases (RUNE-313). Order is the contract; see each phase's doc
	// comment for what pins it. main() reads as the sequence, nothing more.
	b := mustInitRuntime()
	logger := b.logger
	ctx := b.ctx
	closers := b.closers

	cp := mustOpenStore(b)
	cp = mustStartControlPlane(b, cp)

	apiServer := cp.api

	n := mustStartNode(b, cp)
	wireNodeEndpoints(b, cp, n)
	metricsServer := startAuxiliarySurfaces(b, n)
	agentStop := n.stop

	// Wait for cancellation
	<-ctx.Done()

	if metricsServer != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = metricsServer.Shutdown(shutdownCtx)
		cancel()
	}

	// Stop agent before API server so subsystems can drain via the
	// control plane if they need to.
	if agentStop != nil {
		agentStop()
	}

	// Gracefully stop the API server (bounded so Ctrl+C does not hang).
	stopDone := make(chan error, 1)
	go func() { stopDone <- apiServer.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			logger.Error("Failed to stop API server", log.Err(err))
		}
	case <-time.After(20 * time.Second):
		logger.Error("API server stop timed out after 20s; exiting anyway")
		os.Exit(1)
	}

	// Pops watch-server -> vip-allocator -> orderedlog -> state-store ->
	// signal-context, the same LIFO the defers produced.
	closers.closeAll(logger)

	logger.Info("Rune server stopped")
}
