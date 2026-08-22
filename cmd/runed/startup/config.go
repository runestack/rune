package startup

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/version"
	"github.com/spf13/viper"
)

// mustInitRuntime runs startup phases 1-2 (RUNE-313): runtime config, logger,
// signal context, and the global viper bind. It also creates the closer stack
// that every later phase pushes onto.
//
// Body moved verbatim from main(); locals are unpacked/packed at the edges so
// the move stays reviewable line-for-line.
func mustInitRuntime(f *Flags) *boot {
	resolveRuntimeConfig(f)

	// Build logger using helper
	logger := buildLogger(f.LogLevel, f.LogFormat, f.PrettyLogs, f.Debug)

	logger.Info("Starting Rune Server", log.Str("version", version.Version))

	// Context with cancellation
	ctx, cancel := setupSignalContext(logger)

	// Teardown order, explicit and reversed at the end of main (RUNE-313).
	// Pushes sit exactly where the matching `defer` used to.
	var closers closerStack
	closers.push("signal-context", cancel)

	// Bind the global viper to the same runefile initRuntimeConfig
	// resolved. Without a runefile we fail fast — production deployments
	// must ship one (see docs); the dev-loop just needs `runefile.toml`
	// in the cwd. Tests use the override hook in initRuntimeConfig.
	resolvedRunefile := resolveRunefilePath(f)
	if resolvedRunefile == "" {
		logger.Error("No runefile found; pass --config or place runefile.{toml,yaml,yml} in cwd or /etc/rune/")
		os.Exit(1)
	}
	viper.SetConfigFile(resolvedRunefile)
	if err := viper.ReadInConfig(); err != nil {
		logger.Error("Failed to read runefile", log.Str("path", resolvedRunefile), log.Err(err))
		os.Exit(1)
	}

	return &boot{flags: f, ctx: ctx, logger: logger, runefile: resolvedRunefile, closers: &closers}
}

func buildLogger(levelStr, formatStr string, pretty, debug bool) log.Logger {
	if pretty {
		formatStr = "text"
	}
	if debug {
		levelStr = "debug"
	}
	var opts []log.LoggerOption
	lvl, err := log.ParseLevel(levelStr)
	if err != nil {
		fmt.Printf("Invalid log level: %s, defaulting to 'info'\n", levelStr)
		lvl = log.InfoLevel
	}
	opts = append(opts, log.WithLevel(lvl))
	switch strings.ToLower(formatStr) {
	case "json":
		opts = append(opts, log.WithFormatter(&log.JSONFormatter{}))
	case "text", "pretty":
		opts = append(opts, log.WithFormatter(&log.TextFormatter{}))
	default:
		fmt.Printf("Invalid log format: %s, defaulting to 'text'\n", formatStr)
		opts = append(opts, log.WithFormatter(&log.TextFormatter{}))
	}
	return log.NewLogger(opts...)
}
func setupSignalContext(logger log.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal, shutting down (press Ctrl+C again to force quit)", log.Str("signal", sig.String()))
		cancel()
		sig = <-sigCh
		logger.Warn("Received second signal, forcing exit", log.Str("signal", sig.String()))
		os.Exit(1)
	}()
	return ctx, cancel
}

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
func resolveRunefilePath(f *Flags) string {
	if f.ConfigFile != "" {
		return f.ConfigFile
	}
	return findRunefile([]string{".", "/etc/rune"})
}

// resolveRuntimeConfig folds runefile and environment values into f for every
// flag the operator did not set explicitly, at the documented precedence
// (flag > env > runefile > default). f carries the effective config from here
// on; nothing else reads the raw command line.
func resolveRuntimeConfig(f *Flags) {
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
	resolvedRunefile := resolveRunefilePath(f)
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
		f.GRPCAddr = v.GetString("server.grpc_address")
	}

	if !cmdFlags["http-addr"] {
		f.HTTPAddr = v.GetString("server.http_address")
	}

	if !cmdFlags["data-dir"] {
		dataDirFromConfig := v.GetString("data_dir")
		if dataDirFromConfig != "" {
			f.DataDir = dataDirFromConfig
		} else {
			f.DataDir = defaultDataDir
		}
	}

	if !cmdFlags["log-level"] {
		f.LogLevel = v.GetString("log.level")
	}

	if !cmdFlags["log-format"] {
		f.LogFormat = v.GetString("log.format")
	}

	if !cmdFlags["debug"] {
		f.Debug = v.GetBool("debug")
	}

	if !cmdFlags["pretty"] {
		f.PrettyLogs = v.GetBool("pretty")
	}

	// Networking layer flag precedence (RUNE-040..067).
	if !cmdFlags["cluster-cidr"] {
		if s := v.GetString("networking.cluster_cidr"); s != "" {
			f.ClusterCIDR = s
		}
	}
	if !cmdFlags["dev-mode"] {
		f.DevMode = v.GetBool("networking.dev_mode")
	}
	if !cmdFlags["metrics-addr"] {
		// Empty string is a valid value (disables metrics) so use IsSet
		// to distinguish "explicitly set to empty" from "unset".
		if v.IsSet("telemetry.metrics_addr") {
			f.MetricsAddr = v.GetString("telemetry.metrics_addr")
		}
	}
	if !cmdFlags["node-role"] {
		f.NodeRole = v.GetString("node.role")
	}
	// Dev mode is single-node by definition. Auto-promote the node to
	// 'edge' so the ingress controller and ACME orchestrator come up
	// without forcing developers to remember a second flag. An explicit
	// --node-role (including --node-role="" to opt out) is respected.
	if f.DevMode && !cmdFlags["node-role"] && f.NodeRole == "" {
		f.NodeRole = "edge"
	}
	if !cmdFlags["ingress-http-addr"] {
		f.IngressHTTP = v.GetString("ingress.http_addr")
	}
	if !cmdFlags["ingress-https-addr"] {
		f.IngressHTTPS = v.GetString("ingress.https_addr")
	}
	if !cmdFlags["acme-directory"] {
		f.ACMEDirectory = v.GetString("acme.directory")
	}
	if !cmdFlags["acme-email"] {
		f.ACMEEmail = v.GetString("acme.email")
	}

	// Dashboard UI flag precedence (RUNE-200). Defaults match config.Default()
	// (enabled, require_tls), so the flag default holds unless the runefile or
	// an explicit flag overrides it.
	if !cmdFlags["ui"] && v.IsSet("ui.enabled") {
		f.UIEnabled = v.GetBool("ui.enabled")
	}
	if !cmdFlags["ui-path"] && v.IsSet("ui.path") {
		f.UIPath = v.GetString("ui.path")
	}
	if !cmdFlags["ui-require-tls"] && v.IsSet("ui.require_tls") {
		f.UIRequireTLS = v.GetBool("ui.require_tls")
	}

	// Final validation and defaults for required parameters
	if f.DataDir == "" {
		f.DataDir = defaultDataDir
	}
}
