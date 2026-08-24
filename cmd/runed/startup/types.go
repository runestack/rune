package startup

import (
	"context"

	"github.com/runestack/rune/internal/agent"
	dnssub "github.com/runestack/rune/internal/agent/dns"
	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/vip"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/orderedlog"
)

// The three startup groups (RUNE-313). They exist for arity and grouping —
// main() carries ~18 cross-phase values — and because they mirror the
// teardown groups, which is what gives shutdown its shape. They are NOT a
// compile-time ordering guarantee: every phase lives in this package, so a
// nil *node compiles fine. Moving out of package main did not change that —
// unexported fields only constrain callers outside the package, and every
// phase is inside it. Ordering is protected by node.started (checked in
// wireNodeEndpoints), by the single ordered registration list in
// startup_node.go, and by the ordering test in cmd/runed.

// boot is what every later phase needs: cancellable context, logger, the
// resolved runefile path, and the teardown stack.
type boot struct {
	// flags is the effective configuration (see Flags): the resolved values,
	// not the raw command line.
	flags    *Flags
	ctx      context.Context
	logger   log.Logger
	runefile string
	closers  *closerStack

	// identity is the persisted node identity, loaded in phase 1 rather
	// than alongside the agent in phase 7. Everything keyed by node —
	// Instance.NodeID, Volume.BoundNode, the node inventory record — must
	// agree on ONE ID (RUNE-301 §4), and the control plane starts
	// reconciling before the agent exists. Reading node-identity.json is a
	// pure file operation with no dependency on any later phase, so it
	// happens first and every phase is handed the same value.
	identity agent.Identity
}

// controlPlane is everything that must exist before the node starts. It holds
// only what a LATER phase reads: values that live and die inside the
// control-plane phase (the Badger handle, the watch server and its registrar,
// the event log) stay local there, so a field here means a real cross-phase
// dependency. cfg is a
// POINTER on purpose: the API-server phase folds the reconciled UI flags onto
// the config the store phase built (main.go's appCfg.UI.* writes), so a
// by-value copy would silently drop them.
type controlPlane struct {
	store   store.Store
	cfg     *config.Config
	olog    orderedlog.OrderedLog
	vip     *vip.Allocator
	observe observe.LogStore
	api     *server.APIServer
}

// node is the per-host agent and the subsystem handles later phases wire back
// into the control plane. dns is legitimately nil when nothing bindable was
// found (dev mode on some hosts), so every reader must nil-check it.
type node struct {
	agent *agent.Agent
	stop  func()
	dns   *dnssub.Subsystem

	// started records that agent.Start has returned. wireNodeEndpoints
	// requires it: the endpoint publisher needs node identity, and reading
	// it before Start would be the ordering bug this design exists to
	// prevent. A runtime check, because package-main types cannot express
	// this at compile time.
	started bool
}
