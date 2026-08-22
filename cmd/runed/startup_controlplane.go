package main

import (
	"fmt"
	"os"
	"path/filepath"

	observebackend "github.com/runestack/rune/pkg/observe/backend"

	"github.com/runestack/rune/internal/config"
	pb "github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/vip"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/orderedlog"
	watchsvc "github.com/runestack/rune/pkg/watch"
	"google.golang.org/grpc"
)

// mustStartControlPlane runs startup phases 5-6 (RUNE-313): OrderedLog, VIP
// allocator, watch server, event log, observability store, then the API
// server — whose Start() constructs and starts the orchestrator and begins
// reconciling.
//
// Two constraints live here:
//   - olog and the event recorder wrap the SAME *badger.DB the store owns, so
//     the closer stack must tear them down before the store (see closerStack).
//   - server.New seeds notReadyMountResolver via buildServerOptions, so the
//     orchestrator's first reconcile always has a resolver. The REAL resolver
//     is installed later, inside subsystem registration — see startup_node.go.
func mustStartControlPlane(b *boot, cp *controlPlane) *controlPlane {
	ctx := b.ctx
	logger := b.logger
	closers := b.closers
	stateStore := cp.store
	appCfg := cp.cfg

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
	// Must close BEFORE the state store: same *badger.DB.
	closers.push("orderedlog", func() { olog.Close() })

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
	closers.push("vip-allocator", func() { vipAllocator.Close() })

	watchServer := watchsvc.NewServer(olog, logger)
	closers.push("watch-server", func() { watchServer.Close() })

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
	// Fold reconciled UI flags onto the typed config so buildServerOptions
	// can map a single struct into server.WithUI (RUNE-200). Handoff knobs
	// have no flags and ride the runefile/config defaults.
	appCfg.UI.Enabled = *uiEnabled
	appCfg.UI.RequireTLS = *uiRequireTLS
	if *uiPath != "" {
		appCfg.UI.Path = *uiPath
	}

	// Native observability (RuneSight). When [observability] is enabled, build
	// the configured log store once and share it between the ObserveService
	// (query + ingest) and the agent forwarder (in-process ingest). Disabled
	// (the default) leaves observeStore nil; the ObserveService then reports
	// enabled=false and `rune logs` stays on the live ephemeral stream.
	var observeStore observe.LogStore
	if appCfg != nil && appCfg.Observability.Enabled {
		observeStore, err = buildObserveStore(appCfg, *dataDir, logger)
		if err != nil {
			logger.Error("Failed to construct observability store", log.Err(err))
			os.Exit(1)
		}
		logger.Info("Native observability enabled",
			log.Str("backend", observeStore.Capabilities().Backend))
	}

	apiServer, err := server.New(buildServerOptions(*grpcAddr, *httpAddr, stateStore, appCfg, logger, vipAllocator, vipAllocator, eventLog, observeStore, watchRegistrar)...)
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

	cp.db = bs.DB()
	cp.olog = olog
	cp.vip = vipAllocator
	cp.watch = watchServer
	cp.watchRegistrar = watchRegistrar
	cp.events = eventLog
	cp.observe = observeStore
	cp.api = apiServer
	return cp
}

func buildServerOptions(grpcAddress, httpAddress string, st store.Store, appCfg *config.Config, logger log.Logger, netSP service.NetworkStatusProvider, vipAlloc service.VIPAllocator, eventLog events.EventLog, observeStore observe.LogStore, extraRegistrars ...func(grpc.ServiceRegistrar)) []server.Option {
	opts := []server.Option{
		server.WithGRPCAddr(grpcAddress),
		server.WithHTTPAddr(httpAddress),
		server.WithStore(st),
		server.WithLogger(logger),
	}
	if eventLog != nil {
		opts = append(opts, server.WithEventLog(eventLog))
	}
	if observeStore != nil {
		opts = append(opts, server.WithObserveStore(observeStore))
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
	// Embedded dashboard + gRPC-Web transcoder (RUNE-200).
	if appCfg != nil {
		opts = append(opts, server.WithUI(server.UIOptions{
			Enabled:        appCfg.UI.Enabled,
			Path:           appCfg.UI.Path,
			HandoffEnabled: appCfg.UI.HandoffEnabled,
			HandoffTTL:     appCfg.UI.HandoffTTL,
			RequireTLS:     appCfg.UI.RequireTLS,
		}))
		// RUNE-201 session lifetimes (zero fields keep defaults).
		opts = append(opts, server.WithSession(server.SessionOptions{
			AccessTTL:   appCfg.Auth.SessionAccessTTL,
			RefreshTTL:  appCfg.Auth.SessionRefreshTTL,
			GraceWindow: appCfg.Auth.SessionGraceWindow,
		}))
	}
	for _, r := range extraRegistrars {
		opts = append(opts, server.WithExtraGRPCRegistrar(r))
	}
	return opts
}

// buildObserveStore constructs the native observability (RuneSight) log store
// from the runefile [observability] block (plan §5). Only called when
// observability is enabled. An empty/embedded backend yields the in-process
// store; clickhouse/loki yield the optional-sink skeletons.
func buildObserveStore(appCfg *config.Config, dataDir string, logger log.Logger) (observe.LogStore, error) {
	o := appCfg.Observability
	// Persist the embedded store to node-local disk under the runed data dir
	// so logs survive a restart; empty dataDir => in-memory only.
	var embeddedDir string
	if dataDir != "" {
		embeddedDir = filepath.Join(dataDir, "observe")
	}
	return observebackend.Open(observebackend.Backend(o.Backend), observebackend.Options{
		Embedded: observebackend.EmbeddedConfig{
			RetentionDays: o.RetentionDays,
			Dir:           embeddedDir,
		},
		Loki: observebackend.LokiConfig{
			BaseURL:  o.Loki.URL,
			TenantID: o.Loki.TenantID,
		},
		ClickHouse: observebackend.ClickHouseConfig{
			DSN:           o.ClickHouse.DSN,
			Database:      o.ClickHouse.Database,
			Table:         o.ClickHouse.Table,
			RetentionDays: o.RetentionDays,
			AutoMigrate:   o.ClickHouse.AutoMigrate,
			StoragePolicy: o.ClickHouse.StoragePolicy,
			S3Volume:      o.ClickHouse.S3Volume,
			HotDays:       o.ClickHouse.HotDays,
		},
		Logger: logger.WithComponent("observe"),
	})
}
