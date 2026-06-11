package service

import (
	"context"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/types"
)

// usageEnrichTimeout bounds the whole stats fan-out for one RPC. The first
// read for a container is a blocking two-sample docker read (~1s); later
// reads are one-shot (<10ms). Concurrency makes the wall cost ≈ the slowest
// single container, so 3s comfortably covers a cold cache without letting a
// wedged daemon stall list RPCs.
const usageEnrichTimeout = 3 * time.Second

// usageEnrichConcurrency caps parallel daemon calls so a large instance
// list doesn't open hundreds of docker API connections at once.
const usageEnrichConcurrency = 8

// enrichInstancesUsage attaches live resource usage (Instance.Usage) to
// every RUNNING instance whose runner implements runner.StatsProvider.
// Best-effort by design: failures and unsupported runners leave Usage nil
// (the dashboard renders that as unknown, not 0). Mutates the instances in
// place; safe for concurrent instances since each goroutine touches only
// its own element.
func enrichInstancesUsage(ctx context.Context, rm manager.IRunnerManager, instances []*types.Instance, logger log.Logger) {
	if rm == nil || len(instances) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, usageEnrichTimeout)
	defer cancel()

	sem := make(chan struct{}, usageEnrichConcurrency)
	var wg sync.WaitGroup
	for _, inst := range instances {
		if inst == nil || inst.Status != types.InstanceStatusRunning || inst.ContainerID == "" {
			continue
		}
		r, err := rm.GetInstanceRunner(inst)
		if err != nil {
			continue
		}
		sp, ok := r.(runner.StatsProvider)
		if !ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(inst *types.Instance, sp runner.StatsProvider) {
			defer wg.Done()
			defer func() { <-sem }()
			u, err := sp.InstanceStats(ctx, inst)
			if err != nil {
				// Debug, not warn: races with container teardown are routine.
				if logger != nil {
					logger.Debug("instance stats unavailable",
						log.Str("instance", inst.ID), log.Err(err))
				}
				return
			}
			inst.Usage = u
		}(inst, sp)
	}
	wg.Wait()
}

// enrichEmbeddedUsage is the []types.Instance (by-value slice) adapter for
// the instances ServiceService embeds on a Service. It builds element
// pointers so the enrichment mutates the slice in place.
func enrichEmbeddedUsage(ctx context.Context, rm manager.IRunnerManager, instances []types.Instance, logger log.Logger) {
	if len(instances) == 0 {
		return
	}
	ptrs := make([]*types.Instance, len(instances))
	for i := range instances {
		ptrs[i] = &instances[i]
	}
	enrichInstancesUsage(ctx, rm, ptrs, logger)
}
