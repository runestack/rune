package cmd

import (
	"fmt"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/types"
)

// nodeAllocatable is what the largest known node can offer a workload.
type nodeAllocatable struct {
	millicores int64
	memBytes   int64
}

// fetchNodeAllocatable asks the server what its nodes can offer. Returns
// nil when the answer is unavailable for any reason — an older server, a
// denied call, a node that has not reported.
//
// Nil means "unknown", and every caller must treat it as such. A cast
// must never be blocked or scolded because a capacity lookup failed;
// the whole point of this check is to be a courtesy.
func fetchNodeAllocatable(api *client.Client) *nodeAllocatable {
	if api == nil {
		return nil
	}
	hc := generated.NewHealthServiceClient(api.Conn())
	ctx, cancel := api.Context()
	defer cancel()

	resp, err := hc.GetHealth(ctx, &generated.GetHealthRequest{ComponentType: "node"})
	if err != nil || resp == nil {
		return nil
	}

	var best nodeAllocatable
	for _, c := range resp.Components {
		r := c.GetResources()
		if r == nil {
			continue
		}
		if mc := int64(r.GetAllocatableCpuCores() * 1000); mc > best.millicores {
			best.millicores = mc
		}
		if m := r.GetAllocatableMemBytes(); m > best.memBytes {
			best.memBytes = m
		}
	}
	if best.millicores == 0 && best.memBytes == 0 {
		return nil
	}
	return &best
}

// capacityWarnings reports requests no known node can satisfy.
//
// A WARNING, not an error, and deliberately so: inventory may legitimately
// be empty at cast time, the server may be older than this CLI, and on a
// cluster a node with room may join in a minute. Refusing here would turn
// a temporary truth into a permanent one.
//
// It compares against ALLOCATABLE rather than capacity. A node advertised
// as 24GB offers rather less once the kernel, runed and the agent are
// accounted for, and a request sized against the advertised number is the
// one that gets placed and then OOM-killed — with the workload blamed
// rather than the arithmetic.
func capacityWarnings(services []*types.Service, alloc *nodeAllocatable) []string {
	if alloc == nil {
		return nil
	}
	var warns []string
	for _, spec := range services {
		if spec == nil {
			continue
		}
		name := spec.Name

		if req := spec.Resources.Memory.Request; req != "" && alloc.memBytes > 0 {
			if want, err := types.ParseMemory(req); err == nil && want > alloc.memBytes {
				warns = append(warns, fmt.Sprintf(
					"capacity: %s requests %s memory; the largest node can offer %s "+
						"(a node's own kernel and agent are not available to workloads). "+
						"It will not be placed until a bigger node joins.",
					name, types.FormatMemory(want), types.FormatMemory(alloc.memBytes)))
			}
		}

		if req := spec.Resources.CPU.Request; req != "" && alloc.millicores > 0 {
			if cores, err := types.ParseCPU(req); err == nil && int64(cores*1000) > alloc.millicores {
				want := int64(cores * 1000)
				warns = append(warns, fmt.Sprintf(
					"capacity: %s requests %s CPU; the largest node can offer %s. "+
						"It will not be placed until a bigger node joins.",
					name, formatMillicores(want), formatMillicores(alloc.millicores)))
			}
		}
	}
	return warns
}

func formatMillicores(m int64) string {
	if m%1000 == 0 {
		return fmt.Sprintf("%d", m/1000)
	}
	return fmt.Sprintf("%dm", m)
}
