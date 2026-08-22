package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	acmesvc "github.com/runestack/rune/pkg/networking/acme"

	dnssub "github.com/runestack/rune/internal/agent/dns"
	volsub "github.com/runestack/rune/internal/agent/volumes"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/runestack/rune/internal/agent"
	"github.com/runestack/rune/internal/agent/dataplane"
	"github.com/runestack/rune/internal/agent/forwarder"
	"github.com/runestack/rune/internal/agent/ingressctl"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/ingress"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// mustStartNode runs startup phase 7 (RUNE-313): the per-node agent, its
// subsystem registration, and agent.Start.
//
// TWO ordering constraints live in here, both invisible from the call site:
//
//  1. Registration order IS start order. agent.Register appends to a slice and
//     agent.Start starts subsystems in that order (internal/agent/agent.go).
//     The order below — dataplane, volumes, forwarder, DNS, ingress — is a
//     contract: ingressctl reads dpRef.Cache(), so moving ingress ahead of the
//     dataplane panics at startup on edge nodes only. Keep this one ordered
//     block; do not scatter these into independently-called functions.
//
//  2. The REAL mount resolver is installed inside the volumes registration
//     below, NOT in the later wiring phase. Registration runs before
//     agent.Start, so the swap from notReadyMountResolver happens as early as
//     possible. Hoisting it into wireNodeEndpoints would move it after
//     agent.Start and widen the window in which the orchestrator treats every
//     volume as "not yet mounted" (RUNE-311 D4, RUNE-313 D2).
//
// Body moved verbatim from main().
func mustStartNode(b *boot, cp *controlPlane) *node {
	ctx := b.ctx
	logger := b.logger
	olog := cp.olog
	apiServer := cp.api
	stateStore := cp.store
	appCfg := cp.cfg
	vipAllocator := cp.vip
	observeStore := cp.observe
	_ = stateStore
	_ = appCfg
	_ = vipAllocator
	_ = observeStore

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

		// Native observability forwarder (RuneSight, plan §4.1). OFF unless
		// [observability].enabled — only registered when a store was wired
		// above. Dual tap: workload logs via the orchestrator (which fans out
		// to runner.GetLogs) + the agent Outbox for system events. Pushes to
		// the in-process ObserveService ingest path (single-node), buffered
		// through a disk spool for at-least-once delivery.
		if observeStore != nil {
			obsSvc := apiServer.GetObserveService()
			spoolPath := filepath.Join(*dataDir, "observe-spool.jsonl")
			spool, serr := forwarder.NewDiskSpool(spoolPath, 0, logger.WithComponent("agent.forwarder.spool"))
			if serr != nil {
				return fmt.Errorf("forwarder spool: %w", serr)
			}
			fwd, ferr := forwarder.New(forwarder.Config{
				Source:   apiServer.GetOrchestrator(),
				Ingester: obsSvc,
				Outbox:   a.Outbox(),
				NodeID:   a.Identity().NodeID,
				Spool:    spool,
				Logger:   logger.WithComponent("agent.forwarder"),
			})
			if ferr != nil {
				return fmt.Errorf("forwarder: %w", ferr)
			}
			if err := a.Register(fwd); err != nil {
				return err
			}
		}

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
		bindAddrs, bindSkipped := dnssub.FilterBindable(bindAddrs, logger)
		if len(bindAddrs) == 0 {
			if *devMode {
				logger.Warn("DNS subsystem skipped: no bindable addresses on this host (common on macOS/Docker Desktop); use dataplane on 127.0.0.1 for host access")
			} else {
				return fmt.Errorf("dns: no bindable addresses: %s", dnssub.DiagnoseEmptyBind(bindSkipped))
			}
		} else {
			dnsSub, derr = dnssub.New(dnssub.Config{
				Zone: dnssub.NewStoreZone(stateStore, dnssub.FuncVIPSource{Fn: func(ctx context.Context, serviceID string) (net.IP, error) {
					return vipAllocator.Allocate(ctx, serviceID)
				}}, logger.WithComponent("dns-zone")),
				UpstreamProvider: dnssub.ResolvConfUpstreams(bindAddrs...),
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
			// Shared per-host client-CA registry for inbound mTLS
			// (origin hardening): the controller writes pools resolved
			// from each service's clientCert.caSecret; the listener reads
			// them at handshake time.
			clientCAs := ingress.NewClientCARegistry()

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
				ClientCAs:         clientCAs,
				Logger:            logger.WithComponent("ingressctl"),
				ReservedHostPorts: ingressReservedPorts(httpAddr, httpsAddr, *devMode),
			})

			isub, ierr := ingress.New(ingress.Config{
				Router:           router,
				Challenges:       challenges,
				Certs:            loader,
				ClientCAs:        clientCAs,
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

	// started=true is what wireNodeEndpoints checks before reading node
	// identity; see its guard.
	return &node{agent: agentInst, stop: agentStop, dns: dnsSub, started: true}
}
