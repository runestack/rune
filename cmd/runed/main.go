package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/runestack/rune/internal/agent"
	"github.com/runestack/rune/internal/agent/dataplane"
	dnssub "github.com/runestack/rune/internal/agent/dns"
	"github.com/runestack/rune/internal/agent/ingressctl"
	volsub "github.com/runestack/rune/internal/agent/volumes"
	"github.com/runestack/rune/internal/config"
	pb "github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	acmesvc "github.com/runestack/rune/pkg/networking/acme"
	"github.com/runestack/rune/pkg/networking/ingress"
	"github.com/runestack/rune/pkg/networking/vip"
	"github.com/runestack/rune/pkg/runner/docker/bridges"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/storage/driverparams"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/version"
	watchsvc "github.com/runestack/rune/pkg/watch"
	"github.com/spf13/viper"

	// Storage drivers — each blank-import registers one or more driver
	// names with pkg/storage/driver.Registry at init() time. Adding a new
	// driver (Hetzner, AWS EBS, ...) is one more line here.
	_ "github.com/runestack/rune/pkg/storage/driver/dovolume"
	_ "github.com/runestack/rune/pkg/storage/driver/local"

	"github.com/runestack/rune/pkg/store/repos"
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

	// Initialize runtime configuration
	initRuntimeConfig()

	// Build logger using helper
	logger := buildLogger(*logLevel, *logFormat, *prettyLogs, *debugLogLevel)

	logger.Info("Starting Rune Server", log.Str("version", version.Version))

	// Context with cancellation
	ctx, cancel := setupSignalContext(logger)
	defer cancel()

	// Bind the global viper to the same runefile initRuntimeConfig
	// resolved. Without a runefile we fail fast — production deployments
	// must ship one (see docs); the dev-loop just needs `runefile.toml`
	// in the cwd. Tests use the override hook in initRuntimeConfig.
	resolvedRunefile := resolveRunefilePath()
	if resolvedRunefile == "" {
		logger.Error("No runefile found; pass --config or place runefile.{toml,yaml,yml} in cwd or /etc/rune/")
		os.Exit(1)
	}
	viper.SetConfigFile(resolvedRunefile)
	if err := viper.ReadInConfig(); err != nil {
		logger.Error("Failed to read runefile", log.Str("path", resolvedRunefile), log.Err(err))
		os.Exit(1)
	}

	// Open state store via helper
	stateStore, appCfg, _, err := openStateStore(logger, resolvedRunefile, *dataDir)
	if err != nil {
		logger.Error("Failed to open state store", log.Err(err))
		os.Exit(1)
	}
	defer stateStore.Close()

	// Dev-mode storage overlay: when --dev-mode is on, force the
	// local/local-host drivers into a laptop-friendly layout —
	// allowCreateMissing=true and a default ~/.rune/volumes root
	// that's mkdir'able under the developer's home (the production
	// /var/lib/rune/volumes default usually requires root). Operator
	// config wins; we never overwrite explicit values.
	if *devMode && appCfg != nil {
		applyDevModeStorageOverlay(&appCfg.Storage, logger)
	}

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

	// Construct the persisted event log (RUNE-126 Phase 2). Sits on the
	// shared Badger handle under its own `events/...` keyspace — no
	// OrderedLog/Raft coupling; events are observability, not consensus.
	eventLog, err := events.NewRecorder(bs.DB(), logger, events.Options{})
	if err != nil {
		logger.Error("Failed to construct event log", log.Err(err))
		os.Exit(1)
	}

	// Create and start API server (with WatchService registered).
	apiServer, err := server.New(buildServerOptions(*grpcAddr, *httpAddr, stateStore, appCfg, logger, vipAllocator, vipAllocator, eventLog, watchRegistrar)...)
	if err != nil {
		logger.Error("Failed to create API server", log.Err(err))
		os.Exit(1)
	}

	if err := apiServer.Start(); err != nil {
		logger.Error("Failed to start API server", log.Err(err))
		os.Exit(1)
	}

	// (notReadyMountResolver is pre-seeded via WithInitialMountResolver
	// in buildServerOptions, before apiServer.Start runs the
	// orchestrator's first reconcile.)

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
		// On an edge node the ingress owns :80/:443; the dataplane must
		// not open VIP listeners there or it collides with the ingress
		// wildcard bind and fails the whole ingress subsystem.
		var reservedPorts []int
		if types.IsEdgeNode(a.Identity().Labels) {
			reservedPorts = ingressReservedPorts(*ingressHTTP, *ingressHTTPS, *devMode)
		}
		dp, derr := dataplane.New(dataplane.Config{
			OrderedLog: olog,
			Store:      stateStore,
			VIPResolver: dataplane.FuncVIPResolver{Fn: func(ctx context.Context, serviceID string) (net.IP, error) {
				return vipAllocator.Allocate(ctx, serviceID)
			}},
			Node:              dataplane.StaticNodeID(a.Identity().NodeID),
			Mode:              dpMode,
			ReservedHostPorts: reservedPorts,
			Logger:            logger,
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

		// Per-node storage subsystem (RUNE-069). Watches Volume rows
		// and, for each volume the orchestrator has bound to this
		// node, calls the registered driver's Attach + Mount under
		// /var/lib/rune/mounts/<volume.ID>/. Symmetrically Unmount +
		// Detach on unbind/delete and on agent Stop. The instance
		// controller's resolveVolumeMount consults the subsystem's
		// MountTargetFor() and falls back to Volume.Handle when no
		// mount is recorded yet (correct for the in-tree local /
		// local-host drivers, since their Mount returns the host
		// path verbatim). The resolver-first path is what makes
		// future block-device drivers (do-volume, ...) usable.
		var driverConfigs map[string]map[string]any
		if appCfg != nil {
			driverConfigs = appCfg.Storage.Drivers
		}
		nodeHostname, _ := os.Hostname()
		volSub, vsErr := volsub.New(volsub.Config{
			Store:        stateStore,
			NodeID:       a.Identity().NodeID,
			NodeHostname: nodeHostname,
			Lookup:       makeAgentDriverLookup(stateStore, driverConfigs),
			SecretLookup: newStoreSecretLookup(stateStore),
			Logger:       logger.WithComponent("agent.volumes"),
		})
		if vsErr != nil {
			return fmt.Errorf("agent volumes: %w", vsErr)
		}
		if err := a.Register(volSub); err != nil {
			return err
		}
		// Wire the subsystem into the orchestrator so the instance
		// controller's resolveVolumeMount asks it for a mount target
		// before falling back to Volume.Handle.
		apiServer.GetOrchestrator().SetMountResolver(volSub)

		// Embedded DNS subsystem (RUNE-063). Registers itself with
		// the agent so it inherits supervised lifecycle. The
		// store-backed ZoneProvider answers <svc>.<ns>.rune;
		// freshness is "always" until the data plane exposes a
		// real accessor.
		//
		// Bind on the loopback default AND every docker bridge
		// gateway: containers see their bridge gateway IP as their
		// default route, so binding the resolver there is what
		// makes `nameserver 172.17.0.1` (injected below) actually
		// reachable from inside a container. Without the bridge
		// binds, containers get `Connection refused` on every
		// lookup — the symptom we hit live on dev.85.
		//
		// FilterBindable drops addresses the host cannot listen on
		// (macOS lacks 127.0.0.123 on loopback; Docker Desktop reports
		// bridge gateways that are not host-bindable). In dev mode we
		// also try a non-privileged loopback port and skip DNS entirely
		// if nothing binds — laptop dev still works via dataplane.
		bindAddrs := dnsBindCandidates(*devMode, logger)
		bindAddrs = dnssub.FilterBindable(bindAddrs, logger)
		if len(bindAddrs) == 0 {
			if *devMode {
				logger.Warn("DNS subsystem skipped: no bindable addresses on this host (common on macOS/Docker Desktop); use dataplane on 127.0.0.1 for host access")
			} else {
				return fmt.Errorf("dns: no bindable addresses")
			}
		} else {
			dnsSub, derr = dnssub.New(dnssub.Config{
				Zone: dnssub.NewStoreZone(stateStore, dnssub.FuncVIPSource{Fn: func(ctx context.Context, serviceID string) (net.IP, error) {
					return vipAllocator.Allocate(ctx, serviceID)
				}}, logger.WithComponent("dns-zone")),
				UpstreamProvider: dnssub.ResolvConfUpstreams(),
				BindAddrs:        bindAddrs,
				Logger:           logger.WithComponent("dns"),
			})
			if derr != nil {
				return fmt.Errorf("dns: %w", derr)
			}
			if err := a.Register(dnsSub); err != nil {
				return err
			}
		}

		// Ingress controller + ACME orchestrator (RUNE-066).
		// Edge-only: any node whose role label contains "edge"
		// terminates :80/:443 and runs the ACME issuer.
		if types.IsEdgeNode(a.Identity().Labels) {
			challenges := ingress.NewMemChallengeStore()
			// BadgerCertStore persists ACME-issued certs across runed
			// restarts so we don't re-issue every cert on boot (which
			// trips the LE per-identifier-set rate limit). Falls back
			// to MemCertStore only if the SecretRepo's KEK isn't
			// available, which would itself be a startup failure
			// upstream of this code path.
			certStore := acmesvc.NewBadgerCertStore(stateStore)
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
				Router:            router,
				Store:             stateStore,
				Cache:             dpRef.Cache(),
				ACME:              orch,
				Secrets:           repos.NewSecretRepo(stateStore),
				Certs:             acmeCertStoreWithReload{store: certStore, loader: loader},
				Logger:            logger.WithComponent("ingressctl"),
				ReservedHostPorts: ingressReservedPorts(httpAddr, httpsAddr, *devMode),
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

	// Tell the docker runner to inject Rune's embedded DNS server
	// into every subsequently-created container (RUNE-063). Without
	// this call, containers inherit the host's /etc/resolv.conf and
	// cannot resolve `<service>.<namespace>.rune` — every
	// cross-service hostname returns NXDOMAIN from upstream DNS.
	// We strip the :port suffix because docker's --dns expects
	// addresses only (it always queries on UDP/TCP 53), and the
	// search domain ensures bare service names ("mongo") resolve
	// inside the same namespace.
	if dnsSub != nil {
		dnsIPs := dnsServerIPs(dnsSub.BindAddrs())
		if len(dnsIPs) > 0 {
			apiServer.GetRunnerManager().SetDNSInjection(dnsIPs, []string{"rune."})
			logger.Info("Container DNS injection enabled",
				log.Str("servers", strings.Join(dnsIPs, ",")))
		} else {
			logger.Warn("DNS subsystem has no bind addresses; container DNS injection skipped")
		}
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

// newStoreSecretLookup returns a SecretRepo-backed SecretLookup that
// resolves a (namespace, name, key) triple to the plaintext value of a
// Rune Secret. Shared by the volume / snapshot controllers and the
// node-side agent volume subsystem so secret-ref-shaped values in
// StorageClass / Volume parameters resolve identically wherever they
// land. See RUNE-200 PR 3 / pkg/storage/driverparams.
//
// The closure performs a fresh lookup on every call so secret rotation
// takes effect on the next reconcile without restarting runed.
func newStoreSecretLookup(st store.Store) driverparams.SecretLookup {
	repo := repos.NewSecretRepo(st)
	return func(ctx context.Context, ns, name, key string) (string, error) {
		sec, err := repo.Get(ctx, ns, name)
		if err != nil {
			return "", err
		}
		v, ok := sec.Data[key]
		if !ok {
			return "", fmt.Errorf("secret %s/%s has no key %q", ns, name, key)
		}
		return v, nil
	}
}

// makeAgentDriverLookup returns a volsub.DriverLookup closure that
// resolves a Volume to its driver by reading the Volume's StorageClass
// from the store and instantiating the driver from the registry, with
// per-driver config drawn from the runefile [storage.drivers] section.
// Driver instances are cached so repeated lookups don't allocate.
func makeAgentDriverLookup(st store.Store, driverConfigs map[string]map[string]any) volsub.DriverLookup {
	var (
		mu      sync.Mutex
		drivers = make(map[string]driver.Driver)
	)
	return func(ctx context.Context, vol *types.Volume) (driver.Driver, error) {
		if vol.StorageClassName == "" {
			return nil, fmt.Errorf("volume %s has no storageClassName", vol.String())
		}
		var sc types.StorageClass
		if err := st.Get(ctx, types.ResourceTypeStorageClass, "", vol.StorageClassName, &sc); err != nil {
			return nil, fmt.Errorf("get storage class %q: %w", vol.StorageClassName, err)
		}
		if sc.Driver == "" {
			return nil, fmt.Errorf("storage class %q has empty driver", sc.Name)
		}
		mu.Lock()
		defer mu.Unlock()
		if d, ok := drivers[sc.Driver]; ok {
			return d, nil
		}
		d, err := driver.New(sc.Driver, driverConfigs[sc.Driver])
		if err != nil {
			return nil, fmt.Errorf("instantiate driver %q: %w", sc.Driver, err)
		}
		drivers[sc.Driver] = d
		return d, nil
	}
}

func buildServerOptions(grpcAddress, httpAddress string, st store.Store, appCfg *config.Config, logger log.Logger, netSP service.NetworkStatusProvider, vipAlloc service.VIPAllocator, eventLog events.EventLog, extraRegistrars ...func(grpc.ServiceRegistrar)) []server.Option {
	opts := []server.Option{
		server.WithGRPCAddr(grpcAddress),
		server.WithHTTPAddr(httpAddress),
		server.WithStore(st),
		server.WithLogger(logger),
	}
	if eventLog != nil {
		opts = append(opts, server.WithEventLog(eventLog))
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
	// Resolver for `secret:...` refs in StorageClass / Volume parameters.
	// See RUNE-200 PR 3 — drivers receive plaintext via OpContext.Parameters
	// regardless of where the ref lives in the parameter chain.
	opts = append(opts, server.WithStorageSecretLookup(newStoreSecretLookup(st)))
	// Pre-seed a never-ready MountResolver so the orchestrator's first
	// reconcile tick (inside apiServer.Start) treats every volume as
	// "not yet mounted — retry" rather than falling back to using
	// Volume.Handle as the bind source. The agent's volumes Subsystem
	// will replace this with its real resolver once it's up.
	opts = append(opts, server.WithInitialMountResolver(notReadyMountResolver{}))
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

// applyDevModeStorageOverlay mutates the operator-provided Storage
// config so the local storage drivers run cleanly on a developer
// laptop under --dev-mode:
//
//   - allowCreateMissing is forced to true on the local-host driver
//     (so MyService.volumes hostPath: ~/proj/data auto-mkdirs instead
//     of failing with ErrInvalidConfig).
//   - ~/.rune/volumes is appended to local-host.hostPathAllowlist if
//     not already present, giving services a default writable area.
//   - local.localVolumeRoot defaults to ~/.rune/volumes when unset
//     (production default is /var/lib/rune/volumes which usually
//     requires root on Linux and never exists on macOS).
//
// Operator-provided values always win — every key is only filled in
// when the operator left it unset. Callers should only invoke this
// when *devMode is true.
func applyDevModeStorageOverlay(s *config.Storage, logger log.Logger) {
	if s == nil {
		return
	}
	if s.Drivers == nil {
		s.Drivers = make(map[string]map[string]any, 2)
	}

	home, _ := os.UserHomeDir()
	devVolRoot := ""
	if home != "" {
		devVolRoot = filepath.Join(home, ".rune", "volumes")
	}

	// local-host driver overlay.
	hostCfg := s.Drivers["local-host"]
	if hostCfg == nil {
		hostCfg = make(map[string]any, 2)
	}
	if _, set := hostCfg["allowCreateMissing"]; !set {
		hostCfg["allowCreateMissing"] = true
	}
	if devVolRoot != "" {
		raw, ok := hostCfg["hostPathAllowlist"]
		var allow []any
		if ok {
			if existing, ok2 := raw.([]any); ok2 {
				allow = existing
			} else if existing, ok2 := raw.([]string); ok2 {
				allow = make([]any, 0, len(existing))
				for _, v := range existing {
					allow = append(allow, v)
				}
			}
		}
		present := false
		for _, v := range allow {
			if s, ok := v.(string); ok && s == devVolRoot {
				present = true
				break
			}
		}
		if !present {
			allow = append(allow, devVolRoot)
		}
		hostCfg["hostPathAllowlist"] = allow
	}
	s.Drivers["local-host"] = hostCfg

	// local (managed) driver overlay.
	if devVolRoot != "" {
		mgr := s.Drivers["local"]
		if mgr == nil {
			mgr = make(map[string]any, 1)
		}
		if _, set := mgr["localVolumeRoot"]; !set {
			mgr["localVolumeRoot"] = devVolRoot
		}
		s.Drivers["local"] = mgr
	}

	logger.Info("Dev-mode storage overlay applied",
		log.Str("dev_volume_root", devVolRoot),
		log.Bool("allow_create_missing", true))
}

// dnsBindCandidates returns the DNS listen addresses to probe before
// starting the embedded resolver.
func dnsBindCandidates(devMode bool, logger log.Logger) []string {
	out := []string{dnssub.DefaultBindAddr}
	if devMode {
		out = append([]string{dnssub.DevDefaultBindAddr}, out...)
	}
	out = append(out, dnsBridgeBindAddrs(logger)...)
	return out
}

// dnsBridgeBindAddrs enumerates docker bridge gateways and returns
// `<ip>:53` for each one. The DNS subsystem binds these alongside
// the host loopback so containers on any docker bridge can reach
// the resolver through their own gateway IP (containers cannot
// reach the host's 127.0.0.123 from inside their network
// namespace). Best-effort: a docker-daemon error is logged and
// returns an empty slice — the agent still binds the loopback so
// host-side tools keep working.
func dnsBridgeBindAddrs(logger log.Logger) []string {
	c, err := bridges.NewClient()
	if err != nil {
		logger.Warn("DNS bridge enumeration: docker client unavailable; container DNS may fail",
			log.Err(err))
		return nil
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gws, err := bridges.EnumerateGateways(ctx, c)
	if err != nil {
		logger.Warn("DNS bridge enumeration failed; container DNS may fail",
			log.Err(err))
		return nil
	}
	out := make([]string, 0, len(gws))
	for _, g := range gws {
		out = append(out, net.JoinHostPort(g.IP.String(), "53"))
	}
	if len(out) == 0 {
		logger.Warn("No docker bridge gateways found; container DNS will rely on loopback only")
	} else {
		logger.Info("DNS bridge gateways discovered",
			log.Str("binds", strings.Join(out, ",")))
	}
	return out
}

// dnsServerIPs strips the :port suffix from each "host:port" bind
// address and returns the container-reachable IPs in stable order.
// docker's --dns flag wants addresses, not host:port pairs (it always
// queries on 53).
//
// Loopback addresses (e.g. the 127.0.0.123 host bind) are dropped:
// inside a container 127.x.x.x is the container's *own* loopback, not
// the host's, so injecting one as a nameserver gives every container a
// dead resolver entry — lookups hit it first and intermittently fail
// with ENOTFOUND. Only bridge-gateway addresses are reachable from a
// container network namespace.
func dnsServerIPs(bindAddrs []string) []string {
	out := make([]string, 0, len(bindAddrs))
	seen := make(map[string]struct{}, len(bindAddrs))
	for _, addr := range bindAddrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || host == "" {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

// ingressReservedPorts returns the host ports the edge ingress binds
// (HTTP + HTTPS). The dataplane must not open VIP listeners on them —
// see dataplane.Config.ReservedHostPorts.
func ingressReservedPorts(httpAddr, httpsAddr string, dev bool) []int {
	httpDefault, httpsDefault := 80, 443
	if dev {
		httpDefault, httpsDefault = 8080, 8443
	}
	return []int{
		addrPortOr(httpAddr, httpDefault),
		addrPortOr(httpsAddr, httpsDefault),
	}
}

// addrPortOr extracts the port from a "host:port" bind address,
// falling back to def when addr is empty or unparseable.
func addrPortOr(addr string, def int) int {
	if addr == "" {
		return def
	}
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return def
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
