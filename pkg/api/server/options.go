package server

import (
	"fmt"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/orchestrator/wiring"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/storage/driverparams"
	"github.com/runestack/rune/pkg/store"
	"google.golang.org/grpc"
)

// Options defines the options for the API server.
type Options struct {
	// Server addresses
	GRPCAddr string
	HTTPAddr string

	// TLS configuration
	EnableTLS   bool
	TLSCertFile string
	TLSKeyFile  string

	// Authentication (token-only)
	EnableAuth bool

	// Logging
	Logger log.Logger

	// State store
	Store store.Store

	// Runner manager
	RunnerManager *manager.RunnerManager

	// Orchestrator
	Orchestrator orchestrator.Orchestrator

	// ExtraGRPCRegistrars are invoked during gRPC server startup to
	// register additional services beyond the built-in set. Used by
	// runed to plug in WatchService (RUNE-028) without forcing the
	// API server to depend on every networking-layer package.
	ExtraGRPCRegistrars []func(grpc.ServiceRegistrar)

	// NetworkStatusProvider, if set, is wired into AdminService so
	// the NetworkStatus RPC can report ClusterNetwork CIDR + VIP
	// allocations. Supplied by runed alongside the VIP allocator
	// (RUNE-040).
	NetworkStatusProvider service.NetworkStatusProvider

	// UpgradeStager, if set, is wired into AdminService so UpgradeServer
	// can stage server upgrades. Unset (dev shells, tests) the RPC
	// answers Unimplemented.
	UpgradeStager service.UpgradeStager

	// VIPAllocator, if set, is plugged into ServiceService so each
	// CreateService call assigns a stable VIP from the pool.
	// Supplied by runed at startup.
	VIPAllocator service.VIPAllocator

	// StorageDriverConfigs is the per-driver opaque configuration
	// map (driver name → key/value) passed to the orchestrator when
	// the server constructs one itself (i.e. when WithOrchestrator
	// was not used). Sourced from the runefile [storage.drivers]
	// table by runed. Nil-safe — drivers fall back to their own
	// defaults.
	StorageDriverConfigs map[string]map[string]any

	// StorageDefaultStorageClass mirrors the runefile [storage].
	// defaultStorageClass knob. *string so empty-string ("no cluster
	// default") is distinguishable from unset.
	StorageDefaultStorageClass *string

	// StorageSecretLookup resolves `secret:...` references inside
	// StorageClass / Volume parameters before they reach the storage
	// drivers. Wired by cmd/runed against the store-backed SecretRepo.
	// See RUNE-200 PR 3 / pkg/storage/driverparams.
	StorageSecretLookup driverparams.SecretLookup

	// StoragePreserveOnDelete mirrors the runefile [storage].
	// preserveOnDelete knob. When true the local driver treats
	// ReclaimPolicy:delete as retain.
	StoragePreserveOnDelete bool

	// InitialMountResolver, if set, is installed on the instance
	// controller before the orchestrator's first reconcile tick. Lets
	// cmd/runed pre-seed a never-ready stub so the production window
	// between orchestrator start and agent.volumes registering the
	// real resolver returns transient "not yet mounted" errors instead
	// of falling back to Volume.Handle.
	InitialMountResolver wiring.MountResolver

	// NodeID is this machine's identity (agent.Identity().NodeID),
	// loaded from node-identity.json before the control plane starts.
	// Threaded to the orchestrator so created instances carry the same
	// node ID as their volumes and log streams, and to the runner manager
	// so container-derived instances agree.
	NodeID string

	// EventLog is the persisted resource event log (RUNE-126 Phase 2).
	// When set, the orchestrator wires it into the instance and volume
	// controllers so status transitions surface in `rune describe`.
	// Nil disables emission.
	EventLog events.EventLog

	// ObserveStore is the native observability (RuneSight) log store
	// selected by the runefile [observability] block. When set, the
	// ObserveService serves history queries and the agent forwarder's
	// in-process ingest path writes to it. Nil means observability is
	// disabled — ObserveService still registers but reports enabled=false
	// so `rune logs` falls back to the live ephemeral stream. Wired by
	// cmd/runed via WithObserveStore.
	ObserveStore observe.LogStore

	// UI configures the embedded dashboard and the HTTP serving layer it
	// rides on (RUNE-200). When UI.Enabled is false the HTTP server is not
	// started at all. Zero value disables the UI; runed populates this from
	// the runefile [ui] block via WithUI.
	UI UIOptions

	// Session tunes RUNE-201 refresh/access lifetimes. Zero fields fall back to
	// the session package defaults.
	Session SessionOptions
}

// SessionOptions configures RUNE-201 token lifetimes. Server-package mirror of
// the [auth] session_* config keys; runed maps one onto the other.
type SessionOptions struct {
	AccessTTL   time.Duration // short-lived access token lifetime (default 15m)
	RefreshTTL  time.Duration // sliding refresh idle window (default 30d)
	GraceWindow time.Duration // concurrent-refresh grace (default 30s)
}

// UIOptions configures the embedded dashboard HTTP layer (RUNE-200). It is
// the server-package mirror of the runefile config.UI block; cmd/runed maps
// one onto the other so this package needn't import internal/config.
type UIOptions struct {
	// Enabled starts the HTTP server (vanguard transcoder + /ui + handoff).
	Enabled bool
	// Path is the dashboard mount point (default "/ui").
	Path string
	// HandoffEnabled enables POST/GET /v1/ui/handoff/{code}.
	HandoffEnabled bool
	// HandoffTTL bounds a one-time handoff code's lifetime.
	HandoffTTL time.Duration
	// RequireTLS, when set with EnableTLS=false, binds the HTTP server to
	// loopback only and logs a warning instead of exposing bearer-token
	// traffic on the wire.
	RequireTLS bool
}

// Option is a function that configures options.
type Option func(*Options)

// DefaultOptions returns the default options.
func DefaultOptions() *Options {
	return &Options{
		GRPCAddr:  fmt.Sprintf(":%d", config.DefaultGRPCPort),
		HTTPAddr:  fmt.Sprintf(":%d", config.DefaultHTTPPort),
		EnableTLS: false,
	}
}

// WithGRPCAddr sets the gRPC address.
func WithGRPCAddr(addr string) Option {
	return func(opts *Options) {
		opts.GRPCAddr = addr
	}
}

// WithHTTPAddr sets the HTTP address.
func WithHTTPAddr(addr string) Option {
	return func(opts *Options) {
		opts.HTTPAddr = addr
	}
}

// WithTLS enables TLS with the given certificate and key files.
func WithTLS(certFile, keyFile string) Option {
	return func(o *Options) {
		o.TLSCertFile = certFile
		o.TLSKeyFile = keyFile
		o.EnableTLS = true
	}
}

// WithAuth enables authentication with the given API keys.
func WithAuth(_ []string) Option { // argument ignored; tokens only
	return func(o *Options) {
		o.EnableAuth = true
	}
}

// WithStore sets the state store.
func WithStore(store store.Store) Option {
	return func(opts *Options) {
		opts.Store = store
	}
}

// WithDockerRunner sets the Docker runner.
func WithDockerRunner(runner runner.Runner) Option {
	return func(opts *Options) {
		// No longer needed - runner is handled by orchestrator
	}
}

// WithProcessRunner sets the process runner.
func WithProcessRunner(runner runner.Runner) Option {
	return func(opts *Options) {
		// No longer needed - runner is handled by orchestrator
	}
}

// WithLogger sets the logger.
func WithLogger(logger log.Logger) Option {
	return func(opts *Options) {
		opts.Logger = logger
	}
}

// WithOrchestrator sets the orchestrator.
func WithOrchestrator(orchestrator orchestrator.Orchestrator) Option {
	return func(opts *Options) {
		opts.Orchestrator = orchestrator
	}
}

// WithExtraGRPCRegistrar appends a function that registers an
// additional gRPC service when the server starts. Callers (e.g.
// runed wiring up WatchService) use this to inject services without
// the API server package having to import them.
func WithExtraGRPCRegistrar(reg func(grpc.ServiceRegistrar)) Option {
	return func(opts *Options) {
		if reg != nil {
			opts.ExtraGRPCRegistrars = append(opts.ExtraGRPCRegistrars, reg)
		}
	}
}

// WithUpgradeStager plugs the stager into AdminService so UpgradeServer can
// stage server upgrades.
func WithUpgradeStager(u service.UpgradeStager) Option {
	return func(opts *Options) {
		opts.UpgradeStager = u
	}
}

// WithNetworkStatusProvider plugs the live VIP allocator into the
// AdminService.NetworkStatus RPC.
func WithNetworkStatusProvider(p service.NetworkStatusProvider) Option {
	return func(opts *Options) {
		opts.NetworkStatusProvider = p
	}
}

// WithVIPAllocator plugs the VIP allocator into ServiceService so
// CreateService assigns a stable VIP from the cluster pool.
func WithVIPAllocator(a service.VIPAllocator) Option {
	return func(opts *Options) {
		opts.VIPAllocator = a
	}
}

// WithStorageDriverConfigs threads per-driver opaque configuration
// from the runefile through to the orchestrator that the API server
// constructs internally. No-op when the caller also supplies an
// already-built orchestrator via WithOrchestrator.
func WithStorageDriverConfigs(cfg map[string]map[string]any) Option {
	return func(opts *Options) {
		opts.StorageDriverConfigs = cfg
	}
}

// WithStorageDefaultStorageClass threads the runefile
// [storage].defaultStorageClass knob through to the orchestrator. Nil
// means "keep built-in default"; pointer-to-empty-string means "no
// cluster default — error on missing storageClassName".
func WithStorageDefaultStorageClass(name *string) Option {
	return func(opts *Options) {
		opts.StorageDefaultStorageClass = name
	}
}

// WithStoragePreserveOnDelete threads the runefile
// [storage].preserveOnDelete knob through to the orchestrator.
func WithStoragePreserveOnDelete(preserve bool) Option {
	return func(opts *Options) {
		opts.StoragePreserveOnDelete = preserve
	}
}

// WithStorageSecretLookup threads a SecretLookup function through to
// the orchestrator's volume + snapshot controllers, where it resolves
// `secret:...` references inside StorageClass / Volume parameter maps
// before drivers see them. cmd/runed wires a store-backed implementation
// here; tests typically pass nil (literal token paths only). See
// RUNE-200 PR 3.
func WithStorageSecretLookup(lookup driverparams.SecretLookup) Option {
	return func(opts *Options) {
		opts.StorageSecretLookup = lookup
	}
}

// WithInitialMountResolver pre-installs a MountResolver on the
// instance controller before the orchestrator's first reconcile.
// cmd/runed passes a never-ready stub so the production startup
// window between orchestrator start and agent.volumes registering
// the real resolver returns transient "not yet mounted" errors —
// rather than falling back to Volume.Handle as the bind source,
// which is a UUID for cloud drivers and a fast-path that's correct
// only for local-driver tests.
func WithInitialMountResolver(resolver wiring.MountResolver) Option {
	return func(opts *Options) {
		opts.InitialMountResolver = resolver
	}
}

// WithNodeID wires this node's identity into the orchestrator and the
// runner manager. Empty keeps types.LocalNodeIDFallback.
func WithNodeID(nodeID string) Option {
	return func(opts *Options) {
		opts.NodeID = nodeID
	}
}

// WithEventLog wires the persisted resource event log (RUNE-126 Phase 2)
// so the orchestrator's controllers emit status-transition events that
// `rune describe` surfaces. Nil disables emission.
func WithEventLog(eventLog events.EventLog) Option {
	return func(opts *Options) {
		opts.EventLog = eventLog
	}
}

// WithObserveStore wires the native observability (RuneSight) log store
// selected by the runefile [observability] block. When set, the ObserveService
// serves history queries and the agent forwarder's in-process ingest writes to
// it. Nil disables observability (the service still registers but reports
// enabled=false).
func WithObserveStore(store observe.LogStore) Option {
	return func(opts *Options) {
		opts.ObserveStore = store
	}
}

// WithUI configures the embedded dashboard HTTP layer (RUNE-200). When
// ui.Enabled is true the server starts an HTTP listener on Options.HTTPAddr
// serving the vanguard transcoder under /grpc, the dashboard under ui.Path,
// and the CLI handoff endpoint.
func WithUI(ui UIOptions) Option {
	return func(opts *Options) {
		opts.UI = ui
	}
}

// WithSession sets RUNE-201 token-lifetime overrides.
func WithSession(s SessionOptions) Option {
	return func(opts *Options) {
		opts.Session = s
	}
}
