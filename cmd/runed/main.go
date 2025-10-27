package main

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
	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/version"
	"github.com/spf13/viper"
)

var (
	configFile    = flag.String("config", "", "Path to runefile.yaml (server configuration)")
	grpcAddr      = flag.String("grpc-addr", ":7863", "gRPC server address")
	httpAddr      = flag.String("http-addr", ":7861", "HTTP server address")
	dataDir       = flag.String("data-dir", "", "Data directory (if not specified, uses OS-specific application data directory)")
	logLevel      = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	debugLogLevel = flag.Bool("debug", false, "Enable debug mode (shorthand for --log-level=debug)")
	logFormat     = flag.String("log-format", "text", "Log format (text, json)")
	prettyLogs    = flag.Bool("pretty", false, "Enable pretty text log format (shorthand for --log-format=text)")
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

// createDefaultRunefile creates a default runefile.yaml in the data dir if none exists
func createDefaultRunefile(dataDir string) error {
	defaultConfig := fmt.Sprintf(`# Default Rune server configuration
# This file was auto-generated on first run

docker:
  registries: []
  fallback_api_version: "1.43"
  negotiation_timeout_seconds: 3

server:
  grpc_address: ":7863"
  http_address: ":7861"

log:
  level: "info"
  format: "text"

secret:
  encryption:
    enabled: true
    kek:
      source: "file"
      file: "kek.b64"

data_dir: "%s"
`, dataDir)

	// Ensure data directory exists and write config there
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	configPath := filepath.Join(dataDir, "runefile.yaml")
	if _, err := os.Stat(configPath); err == nil {
		return nil
	}
	return os.WriteFile(configPath, []byte(defaultConfig), 0600)
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

	// 2. Try to load config file if specified or look in standard locations
	configFileSpecified := *configFile != ""
	if configFileSpecified {
		v.SetConfigFile(*configFile)
	} else {
		v.SetConfigName("runefile")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")          // Local development override
		v.AddConfigPath("/etc/rune/") // System-wide production config
	}

	// Read config file if available
	if err := v.ReadInConfig(); err != nil {
		if configFileSpecified {
			// Only show an error if user explicitly specified a config file
			fmt.Printf("Error reading config file %s: %s\n", *configFile, err)
		} else if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			// Show non-"not found" errors even for auto-discovered config
			fmt.Printf("Error reading config file: %s\n", err)
		}
	} else {
		fmt.Printf("Using config file: %s\n", v.ConfigFileUsed())
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

	// Final validation and defaults for required parameters
	if *dataDir == "" {
		*dataDir = defaultDataDir
	}
}

func main() {
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

	// Initialize runtime configuration
	initRuntimeConfig()

	// Build logger using helper
	logger := buildLogger(*logLevel, *logFormat, *prettyLogs, *debugLogLevel)

	logger.Info("Starting Rune Server", log.Str("version", version.Version))

	// Context with cancellation
	ctx, cancel := setupSignalContext(logger)
	defer cancel()

	// Create default runefile if none exists
	if err := createDefaultRunefile(*dataDir); err != nil {
		logger.Warn("Failed to create default runefile", log.Err(err))
		// Don't fail startup, just warn
	}

	// Ensure global viper is bound to the same config file so runtime writes persist
	configPath := *configFile
	if configPath == "" {
		configPath = filepath.Join(*dataDir, "runefile.yaml")
	}
	viper.SetConfigFile(configPath)
	_ = viper.ReadInConfig()

	// Open state store via helper
	stateStore, appCfg, _, err := openStateStore(logger, *configFile, *dataDir)
	if err != nil {
		logger.Error("Failed to open state store", log.Err(err))
		os.Exit(1)
	}
	defer stateStore.Close()

	// Bootstrap and resolve registry secrets into viper before runner init
	if err := bootstrapAndResolveRegistryAuth(appCfg, stateStore, logger); err != nil {
		logger.Error("Failed to bootstrap/resolve registry auth", log.Err(err))
		os.Exit(1)
	}

	// Token-based auth is always enabled in MVP
	logger.Info("Authentication enabled (token-based)")

	// Create and start API server
	apiServer, err := server.New(buildServerOptions(*grpcAddr, *httpAddr, stateStore, logger)...)
	if err != nil {
		logger.Error("Failed to create API server", log.Err(err))
		os.Exit(1)
	}

	if err := apiServer.Start(); err != nil {
		logger.Error("Failed to start API server", log.Err(err))
		os.Exit(1)
	}

	// Wait for cancellation
	<-ctx.Done()

	// Gracefully stop the API server
	if err := apiServer.Stop(); err != nil {
		logger.Error("Failed to stop API server", log.Err(err))
	}

	logger.Info("Rune server stopped")
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
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal", log.Str("signal", sig.String()))
		cancel()
	}()
	return ctx, cancel
}

func openStateStore(logger log.Logger, cfgFile, dataDirPath string) (store.Store, *config.Config, string, error) {
	storeDir := filepath.Join(dataDirPath, "store")
	if err := os.MkdirAll(storeDir, 0755); err != nil {
		return nil, nil, storeDir, fmt.Errorf("create data dir: %w", err)
	}
	appCfg, _ := config.Load(cfgFile)
	if appCfg.Secret.Encryption.KEK.Source == "file" && appCfg.Secret.Encryption.KEK.File == "" {
		appCfg.Secret.Encryption.KEK.File = filepath.Join(dataDirPath, "kek.b64")
	}
	st := store.NewBadgerStoreWithOptions(logger, store.StoreOptions{
		Path:                    storeDir,
		SecretEncryptionEnabled: appCfg.Secret.Encryption.Enabled,
		KEKOptions:              appCfg.KEKOptions(),
		SecretLimits:            appCfg.Secret.Limits,
		ConfigLimits:            appCfg.ConfigResource.Limits,
	})
	if err := st.Open(storeDir); err != nil {
		return nil, nil, storeDir, err
	}
	return st, appCfg, storeDir, nil
}

func buildServerOptions(grpcAddress, httpAddress string, st store.Store, logger log.Logger) []server.Option {
	opts := []server.Option{
		server.WithGRPCAddr(grpcAddress),
		server.WithHTTPAddr(httpAddress),
		server.WithStore(st),
		server.WithLogger(logger),
	}
	// Token-based auth (MVP)
	opts = append(opts, server.WithAuth(nil))
	return opts
}
