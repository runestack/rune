package releasectl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// teardownError is Apply refusing to write a spec onto a service that is
// being deleted: the reconciler would delete it along with the record.
type teardownError struct {
	ref        types.OwnerRef
	finalizers []types.FinalizerType
	waited     time.Duration
}

func (e *teardownError) Error() string {
	msg := fmt.Sprintf("%s is being deleted", e.ref.Key())
	if e.waited > 0 {
		msg += fmt.Sprintf(" and was not gone after %s", e.waited)
	}
	if len(e.finalizers) > 0 {
		names := make([]string, len(e.finalizers))
		for i, f := range e.finalizers {
			names[i] = string(f)
		}
		msg += fmt.Sprintf(" (cleanup still pending: %s)", strings.Join(names, ", "))
	}
	return msg + "; cast again once teardown finishes"
}

// waitForTeardown polls until the service being deleted is gone, or is back
// without a deletion in progress, so the caller can apply again. Past
// teardownWait it gives up with the cleanup steps still pending.
func (c *Controller) waitForTeardown(ctx context.Context, td *teardownError) error {
	c.log.Info("Cast waiting for service teardown",
		log.Str("service", td.ref.Key()), log.Duration("limit", c.teardownWait))
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, c.teardownWait)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		svc, err := c.orch.GetService(ctx, td.ref.Namespace, td.ref.Name)
		switch {
		case errors.Is(err, orchestrator.ErrServiceNotFound) || store.IsNotFoundError(err):
			return nil
		case err == nil && (svc.Metadata == nil || svc.Metadata.DeletionTimestamp == nil):
			return nil
		case err == nil:
			td.finalizers = svc.Metadata.Finalizers
		}
		select {
		case <-ctx.Done():
			if err := parent.Err(); err != nil {
				return err
			}
			td.waited = c.teardownWait
			return td
		case <-ticker.C:
		}
	}
}
