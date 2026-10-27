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
	id         string    // the record being torn down
	since      time.Time // its DeletionTimestamp
	finalizers []types.FinalizerType
	waited     time.Duration
	lookupErr  error // the last failed lookup while waiting, if any
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
	if e.lookupErr != nil {
		msg += fmt.Sprintf(" (last lookup failed: %v)", e.lookupErr)
	}
	return msg + fmt.Sprintf("; cast again once teardown finishes, or if its cleanup is stuck, give it up with "+
		"`rune delete service %s -n %s --abandon-finalizers`", e.ref.Name, e.ref.Namespace)
}

// waitForTeardown polls until the service being deleted is gone, or the same
// record is back without a deletion in progress, so the caller can apply
// again. Past teardownWait, or when the caller's context ends, it gives up
// naming the cleanup steps still pending.
func (c *Controller) waitForTeardown(ctx context.Context, td *teardownError) error {
	c.log.Info("Cast waiting for service teardown",
		log.Str("service", td.ref.Key()), log.Duration("limit", c.teardownWait))
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, c.teardownWait)
	defer cancel()
	ticker := time.NewTicker(c.teardownPoll)
	defer ticker.Stop()
	for {
		svc, err := c.orch.GetService(ctx, td.ref.Namespace, td.ref.Name)
		switch {
		case errors.Is(err, orchestrator.ErrServiceNotFound) || store.IsNotFoundError(err):
			return nil
		case err != nil:
			// A lookup cut off by the wait's own deadline says nothing about
			// the store.
			if ctx.Err() == nil {
				td.lookupErr = err
			}
		case svc.Metadata == nil || svc.Metadata.DeletionTimestamp == nil:
			// Gone and recreated, or the teardown lifted; the retry checks
			// which.
			return nil
		default:
			td.finalizers = svc.Metadata.Finalizers
			td.lookupErr = nil
		}
		select {
		case <-ctx.Done():
			if err := parent.Err(); err != nil {
				return fmt.Errorf("%s: %w", td.Error(), err)
			}
			td.waited = c.teardownWait
			return td
		case <-ticker.C:
		}
	}
}
