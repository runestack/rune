package main

import (
	"fmt"
	"os"

	pb "github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/server"
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
