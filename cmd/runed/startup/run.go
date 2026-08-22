package startup

import (
	"context"
	"os"
	"time"

	"github.com/runestack/rune/pkg/log"
)

// Run executes the daemon: every startup phase in order, then a blocking wait
// on the signal context, then teardown in the reverse order the phases built.
//
// The sequence is the contract; see each phase's doc comment for what pins it.
// This function reads as that sequence, nothing more.
func Run(f *Flags) {
	b := mustInitRuntime(f)
	logger := b.logger
	ctx := b.ctx
	closers := b.closers

	cp := mustOpenStore(b)
	cp = mustStartControlPlane(b, cp)

	apiServer := cp.api

	n := mustStartNode(b, cp)
	wireNodeEndpoints(b, cp, n)
	metricsServer := startAuxiliarySurfaces(b, n)
	agentStop := n.stop

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

	// Teardown last, and AFTER the log line: the original logged "Rune server
	// stopped" and only then unwound its defers, so a closer that blocks (a
	// watch server holding a client stream, say) must not swallow the line
	// operators grep for to confirm a clean exit.
	//
	// Pops watch-server -> vip-allocator -> orderedlog -> state-store ->
	// signal-context, the same LIFO the defers produced.
	closers.closeAll(logger)
}
