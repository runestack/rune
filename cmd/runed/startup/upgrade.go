package startup

import (
	"context"
	"runtime"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/upgrade"
)

// newUpgradeStager builds the RUNE-321 stager. Constructed unconditionally
// on linux — whether this deployment can actually self-upgrade (applier
// units installed, path unit watching this data dir) is checked per
// staging call, so a dev shell answers with a precise FailedPrecondition
// rather than being silently un-wired.
func newUpgradeStager(dataDir, nodeID string, eventLog events.EventLog, logger log.Logger) *upgrade.Stager {
	if runtime.GOOS != "linux" {
		return nil
	}
	return &upgrade.Stager{
		DataDir:  dataDir,
		NodeID:   nodeID,
		EventLog: eventLog,
		Logger:   logger.WithComponent("upgrade-stager"),
	}
}

// watchUpgradeResult polls for the applier's result file and emits the
// matching event within seconds. The applier cannot emit events itself
// (it is a separate root process, and for a failed-before-restart apply
// runed never restarts), and emitting only at next startup would sit on a
// failure until some unrelated restart weeks later surfaces a stale alarm.
//
// The file lives in the root-owned workdir, which is what makes it
// unforgeable by this (unprivileged) process's own account; ownership is
// re-checked before trusting the content.
func watchUpgradeResult(ctx context.Context, eventLog events.EventLog, nodeID string, logger log.Logger) {
	logger = logger.WithComponent("upgrade-result-watch")
	var lastSeen time.Time
	first := true

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		res, fi, err := upgrade.ReadResult()
		if err != nil {
			continue
		}
		if !upgrade.ResultOwnedByRoot(fi) {
			continue
		}
		mtime := fi.ModTime()
		if !mtime.After(lastSeen) {
			continue
		}
		// On the first observation (typically right after the restart an
		// apply just performed) only a recent result is news; an old one
		// was already reported by a previous incarnation.
		if first && time.Since(mtime) > 10*time.Minute {
			lastSeen = mtime
			first = false
			continue
		}
		first = false
		lastSeen = mtime

		level := types.EventLevelInfo
		reason := "UpgradeApplied"
		switch res.Outcome {
		case "rolled-back":
			level, reason = types.EventLevelWarn, "UpgradeRolledBack"
		case "failed":
			level, reason = types.EventLevelWarn, "UpgradeFailed"
		case "noop":
			reason = "UpgradeSkipped"
		}
		msg := "Server upgrade " + res.Outcome + ": " + res.FromVersion + " -> " + res.ToVersion
		if res.Reason != "" {
			msg += " (" + res.Reason + ")"
		}
		now := time.Now().UTC()
		if err := eventLog.Emit(ctx, types.Event{
			Kind: "Node", Name: nodeID, Level: level, Reason: reason, Message: msg,
			FirstSeen: now, LastSeen: now, Count: 1,
		}); err != nil {
			logger.Warn("Failed to emit upgrade result event", log.Err(err))
		}
		logger.Info("Upgrade result observed", log.Str("outcome", res.Outcome), log.Str("to", res.ToVersion))
	}
}
