package server

import (
	"fmt"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/runner/manager"
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

// WithNetworkStatusProvider plugs the live VIP allocator into the
// AdminService.NetworkStatus RPC.
func WithNetworkStatusProvider(p service.NetworkStatusProvider) Option {
	return func(opts *Options) {
		opts.NetworkStatusProvider = p
	}
}
