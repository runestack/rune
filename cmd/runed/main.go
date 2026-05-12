package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/runestack/rune/internal/agent"
	"github.com/runestack/rune/internal/agent/dataplane"
	dnssub "github.com/runestack/rune/internal/agent/dns"
	"github.com/runestack/rune/internal/agent/ingressctl"
	"github.com/runestack/rune/internal/config"
	pb "github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/log"
	acmesvc "github.com/runestack/rune/pkg/networking/acme"
	"github.com/runestack/rune/pkg/networking/ingress"
	"github.com/runestack/rune/pkg/networking/vip"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/version"
	watchsvc "github.com/runestack/rune/pkg/watch"
	"github.com/spf13/viper"

	// Storage drivers — each blank-import registers one or more driver
	// names with pkg/storage/driver.Registry at init() time. Adding a new
	// driver (Hetzner, AWS EBS, ...) is one more line here.
	_ "github.com/runestack/rune/pkg/storage/driver/local"
	"google.golang.org/grpc"
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

// createDefaultRunefile creates a default runefile.yaml in the data dir if none exists.
// If a runefile.toml or runefile.yml already exists alongside, it is left untouched —
// runefile auto-discovery searches for any of {toml,yaml,yml}.
func createDefaultRunefile(dataDir string) error {
	defaultConfig := fmt.Sprintf(`# Default Rune server configuration
# This file was auto-generated on first run.
#
# Rune supports YAML and TOML runefiles. Auto-discovery looks for
# runefile.{toml,yaml,yml} in the working directory, /etc/rune/, and
# the data directory. Use --config <path> to point at a specific file.

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

# Networking layer. All keys are optional; the values
# below are the built-in defaults shown for documentation.
#
# networking:
#   cluster_cidr: "10.96.0.0/16"   # service VIP allocation range
#   dev_mode: false                # skip nftables, bind ingress on user ports,
#                                  # resolve .rune to 127.0.0.1
#
# telemetry:
#   metrics_addr: "127.0.0.1:9100" # Prometheus /metrics endpoint, "" disables
#                                  # (covers all subsystems, not just networking)
#
# node:
#   role: ""                       # comma-separated; "edge" enables ingress + ACME
#
# ingress:
#   http_addr: ""                  # default :80 (or :8080 in dev mode)
#   https_addr: ""                 # default :443 (or :8443 in dev mode), "" disables TLS
#
# acme:
#   directory: ""                  # default Let's Encrypt production
#   email: ""                      # contact email for ACME registration

data_dir: "%s"
`, dataDir)

	// Ensure data directory exists and write config there
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// If any runefile already exists in the data dir (any supported
	// extension), leave it alone.
	for _, ext := range []string{"yaml", "yml", "toml"} {
		if _, err := os.Stat(filepath.Join(dataDir, "runefile."+ext)); err == nil {
			return nil
		}
	}
	return os.WriteFile(filepath.Join(dataDir, "runefile.yaml"), []byte(defaultConfig), 0600)
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

	// 2. Try to load config file if specified or look in standard locations.
	// Both YAML and TOML are supported; viper picks the parser from the
	// file extension when SetConfigFile is used.
	configFileSpecified := *configFile != ""
	if configFileSpecified {
		v.SetConfigFile(*configFile)
	} else {
		// Search for runefile.{toml,yaml,yml} in priority order:
		//   1. Current working directory   (developer override)
		//   2. /etc/rune/                   (system-wide production config)
		//   3. <data_dir>                   (auto-generated default)
		dataDirSearch := defaultDataDir
		if envDD := os.Getenv("RUNE_DATA_DIR"); envDD != "" {
			dataDirSearch = envDD
		}
		if found := findRunefile([]string{".", "/etc/rune", dataDirSearch}); found != "" {
			v.SetConfigFile(found)
		} else {
			// No file present yet (will be auto-created later);
			// keep the legacy yaml lookup to avoid breaking the
			// "first run" code path.
			v.SetConfigName("runefile")
			v.SetConfigType("yaml")
			v.AddConfigPath(".")
			v.AddConfigPath("/etc/rune/")
		}
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

	// Open the in-process OrderedLog before the API server so we can
	// register the WatchService alongside the other gRPC services.
	bs, ok := stateStore.(*store.BadgerStore)
	if !ok {
		logger.Error("State store is not a *BadgerStore", log.Str("type", fmt.Sprintf("%T", stateStore)))
		os.Exit(1)
	}
	olog := orderedlog.NewBadgerBackend(bs.DB(), orderedlog.BackendOptions{
		Logger: logger.WithComponent("orderedlog"),
	})
	if err := olog.Open(); err != nil {
		logger.Error("Failed to open orderedlog", log.Err(err))
		os.Exit(1)
	}
	defer olog.Close()

	// Construct the cluster VIP allocator (RUNE-040). Bootstrapping the
	// CIDR through the OrderedLog is idempotent — re-running with the
	// same CIDR succeeds; a different CIDR after first bootstrap is
	// rejected to protect the persisted ClusterNetwork state.
	vipAllocator, err := vip.New(olog, vip.Options{
		CIDR:   *clusterCIDR,
		Logger: logger.WithComponent("vip-allocator"),
	})
	if err != nil {
		logger.Error("Failed to create VIP allocator", log.Err(err))
		os.Exit(1)
	}
	if err := vipAllocator.Bootstrap(ctx); err != nil {
		logger.Error("Failed to bootstrap cluster network", log.Err(err), log.Str("cidr", *clusterCIDR))
		os.Exit(1)
	}
	defer vipAllocator.Close()

	watchServer := watchsvc.NewServer(olog, logger)
	defer watchServer.Close()

	watchRegistrar := func(reg grpc.ServiceRegistrar) {
		pb.RegisterWatchServiceServer(reg, watchServer)
	}

	// Create and start API server (with WatchService registered).
	apiServer, err := server.New(buildServerOptions(*grpcAddr, *httpAddr, stateStore, appCfg, logger, vipAllocator, vipAllocator, watchRegistrar)...)
	if err != nil {
		logger.Error("Failed to create API server", log.Err(err))
		os.Exit(1)
	}

	if err := apiServer.Start(); err != nil {
		logger.Error("Failed to start API server", log.Err(err))
		os.Exit(1)
	}

	// Start the per-node agent. On single-node, the agent runs in-process
	// and shares the control plane's Badger DB via the in-process
	// OrderedLog backend opened above. Subsystems (data plane, DNS,
	// policy, ingress) register themselves in subsequent
	// networking-layer tickets.
	var dnsSub *dnssub.Subsystem
	var dpRef *dataplane.Subsystem
	extraLabels := map[string]string{}
	if *nodeRole != "" {
		extraLabels[types.LabelNodeRole] = *nodeRole
	}
	agentInst, agentStop, err := startAgent(ctx, logger, olog, *dataDir, *devMode, extraLabels, func(a *agent.Agent) error {
		dpMode := dataplane.ModeProduction
		if *devMode {
			dpMode = dataplane.ModeDev
		}
		dp, derr := dataplane.New(dataplane.Config{
			OrderedLog: olog,
			Node:       dataplane.StaticNodeID(a.Identity().NodeID),
			Mode:       dpMode,
			Logger:     logger,
		})
		if derr != nil {
			return fmt.Errorf("dataplane: %w", derr)
		}
		if err := dp.Metrics().Register(prometheus.DefaultRegisterer); err != nil {
			return fmt.Errorf("dataplane metrics: %w", err)
		}
		if err := a.Register(dp); err != nil {
			return err
		}
		dpRef = dp

		// Embedded DNS subsystem (RUNE-063). Registers itself with
		// the agent so it inherits supervised lifecycle. Bind list
		// stays at the loopback default in MVP; bridge enumeration
		// is done by the caller in a follow-up commit. The store-
		// backed ZoneProvider answers <svc>.<ns>.rune; freshness is
		// "always" until the data plane exposes a real accessor.
		dnsSub, derr = dnssub.New(dnssub.Config{
			Zone:             dnssub.NewStoreZone(stateStore, logger.WithComponent("dns-zone")),
			UpstreamProvider: dnssub.ResolvConfUpstreams(),
			Logger:           logger.WithComponent("dns"),
		})
		if derr != nil {
			return fmt.Errorf("dns: %w", derr)
		}
		if err := a.Register(dnsSub); err != nil {
			return err
		}

		// Ingress controller + ACME orchestrator (RUNE-066).
		// Edge-only: any node whose role label contains "edge"
		// terminates :80/:443 and runs the ACME issuer.
		if types.IsEdgeNode(a.Identity().Labels) {
			challenges := ingress.NewMemChallengeStore()
			certStore := acmesvc.NewMemCertStore()
			loader := ingress.NewCertLoader(certStore)
			router := ingress.NewRouter()

			httpAddr := *ingressHTTP
			httpsAddr := *ingressHTTPS
			if httpAddr == "" {
				if *devMode {
					httpAddr = ":8080"
				} else {
					httpAddr = ":80"
				}
			}
			if httpsAddr == "" {
				if *devMode {
					httpsAddr = ":8443"
				} else {
					httpsAddr = ":443"
				}
			}

			// ACME orchestrator. Single-node = always leader.
			issuer := &acmesvc.HTTP01Issuer{
				Directory:  *acmeDirectory,
				Email:      *acmeEmail,
				Challenges: challenges,
			}
			orch := acmesvc.New(acmesvc.Config{
				Issuer: issuer,
				Certs:  acmeCertStoreWithReload{store: certStore, loader: loader},
				Status: acmeNoopStatus{logger: logger.WithComponent("acme")},
				Logger: logger.WithComponent("acme"),
			})

			// Ingress route reconciler + upstream resolver. Watches
			// the service store, builds a Route per service with
			// `expose.host`, applies them to the Router, and answers
			// the listener's UpstreamResolver lookups out of the
			// dataplane endpoint cache. Without this, the route
			// table stays empty and inbound requests 404.
			ictl := ingressctl.New(ingressctl.Config{
				Router: router,
				Store:  stateStore,
				Cache:  dpRef.Cache(),
				ACME:   orch,
				Logger: logger.WithComponent("ingressctl"),
			})

			isub, ierr := ingress.New(ingress.Config{
				Router:           router,
				Challenges:       challenges,
				Certs:            loader,
				HTTPAddr:         httpAddr,
				HTTPSAddr:        httpsAddr,
				UpstreamResolver: ictl,
				Logger:           logger.WithComponent("ingress"),
			})
			if ierr != nil {
				return fmt.Errorf("ingress: %w", ierr)
			}
			if err := a.Register(isub); err != nil {
				return err
			}

			go func() {
				if err := orch.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Warn("acme orchestrator stopped", log.Err(err))
				}
			}()
			go ictl.Run(ctx)
			logger.Info("Ingress + ACME enabled (edge node)",
				log.Str("http", httpAddr),
				log.Str("https", httpsAddr))
		} else {
			// Visibility for the most common operator footgun: starting
			// runed without --node-role=edge means services with an
			// `expose:` block will run but won't be reachable from
			// outside the cluster. Surface it in logs so the cause is
			// obvious instead of silently dropping ingress traffic.
			logger.Info("Ingress + ACME disabled (non-edge node). Services with `expose:` will not be reachable on this node. Set --node-role=edge (or node.role=edge in the runefile) to enable.")
		}
		return nil
	})
	if err != nil {
		logger.Error("Failed to start agent", log.Err(err))
		_ = apiServer.Stop()
		os.Exit(1)
	}
	_ = agentInst

	// Wire the orchestrator's instance controller to the OrderedLog-
	// backed networking publishers (RUNE-063). Best-effort: if either
	// op-kind has already been registered (dnsSub.New just did so via
	// the agent.Register path), Register is idempotent.
	if pub, perr := dnssub.NewEndpointPublisher(olog, logger.WithComponent("endpoint-publisher")); perr != nil {
		logger.Warn("Endpoint publisher disabled", log.Err(perr))
	} else {
		apiServer.GetOrchestrator().SetEndpointPublisher(pub, agentInst.Identity().NodeID)
	}

	// Optional: serve Prometheus metrics on a private address so
	// scrapers can collect dataplane + future subsystem metrics.
	var metricsServer *http.Server
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		metricsServer = &http.Server{
			Addr:              *metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logger.Info("Metrics server listening", log.Str("addr", *metricsAddr))
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Warn("Metrics server stopped", log.Err(err))
			}
		}()
	}

	// SIGHUP triggers a re-read of upstream DNS resolvers (RUNE-063).
	// Useful when /etc/resolv.conf changes (DHCP renewal, NetworkManager
	// reload, etc.) without restarting runed.
	if dnsSub != nil {
		hupCh := make(chan os.Signal, 1)
		signal.Notify(hupCh, syscall.SIGHUP)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-hupCh:
					if err := dnsSub.Refresh(); err != nil {
						logger.Warn("DNS refresh failed", log.Err(err))
					} else {
						logger.Info("DNS upstreams refreshed (SIGHUP)")
					}
				}
			}
		}()
	}

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

func buildServerOptions(grpcAddress, httpAddress string, st store.Store, appCfg *config.Config, logger log.Logger, netSP service.NetworkStatusProvider, vipAlloc service.VIPAllocator, extraRegistrars ...func(grpc.ServiceRegistrar)) []server.Option {
	opts := []server.Option{
		server.WithGRPCAddr(grpcAddress),
		server.WithHTTPAddr(httpAddress),
		server.WithStore(st),
		server.WithLogger(logger),
	}
	// Token-based auth (MVP)
	opts = append(opts, server.WithAuth(nil))
	if netSP != nil {
		opts = append(opts, server.WithNetworkStatusProvider(netSP))
	}
	if vipAlloc != nil {
		opts = append(opts, server.WithVIPAllocator(vipAlloc))
	}
	// Thread per-driver storage config from the runefile
	// through to the orchestrator (e.g. local.localVolumeRoot,
	// local-host.hostPathAllowlist).
	if appCfg != nil && len(appCfg.Storage.Drivers) > 0 {
		opts = append(opts, server.WithStorageDriverConfigs(appCfg.Storage.Drivers))
	}
	// Thread typed [storage] knobs (defaultStorageClass,
	// preserveOnDelete) through to the volume controller. Each is
	// only set when the operator explicitly supplied it; nil/false
	// preserve the built-in defaults.
	if appCfg != nil && appCfg.Storage.DefaultStorageClass != nil {
		opts = append(opts, server.WithStorageDefaultStorageClass(appCfg.Storage.DefaultStorageClass))
	}
	if appCfg != nil && appCfg.Storage.PreserveOnDelete {
		opts = append(opts, server.WithStoragePreserveOnDelete(true))
	}
	for _, r := range extraRegistrars {
		opts = append(opts, server.WithExtraGRPCRegistrar(r))
	}
	return opts
}

// startAgent boots the per-node agent against an already-open
// OrderedLog. The orderedlog is owned by the caller (main) so it can
// also be shared with the API server's WatchService. Returns the
// agent and a stop function the caller invokes during shutdown.
func startAgent(ctx context.Context, logger log.Logger, olog orderedlog.OrderedLog, dataDirPath string, dev bool, extraLabels map[string]string, registerSubsystems func(*agent.Agent) error) (*agent.Agent, func(), error) {
	identity, err := agent.LoadOrCreateIdentity(dataDirPath)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: load identity: %w", err)
	}
	if len(extraLabels) > 0 {
		if identity.Labels == nil {
			identity.Labels = make(map[string]string, len(extraLabels))
		}
		for k, v := range extraLabels {
			identity.Labels[k] = v
		}
	}

	mode := agent.ModeProduction
	if dev {
		mode = agent.ModeDev
	}

	a, err := agent.New(agent.Config{
		Identity:   identity,
		OrderedLog: olog,
		Mode:       mode,
		Logger:     logger,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("agent: construct: %w", err)
	}

	if registerSubsystems != nil {
		if err := registerSubsystems(a); err != nil {
			return nil, nil, fmt.Errorf("agent: register subsystems: %w", err)
		}
	}

	if err := a.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("agent: start: %w", err)
	}

	logger.Info("Agent started",
		log.Str("node_id", identity.NodeID),
		log.Str("hostname", identity.Hostname),
		log.Str("mode", string(mode)),
	)

	stop := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.Stop(stopCtx); err != nil {
			logger.Warn("Agent stop returned error", log.Err(err))
		}
	}
	return a, stop, nil
}
