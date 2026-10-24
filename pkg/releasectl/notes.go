package releasectl

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
)

// serviceNotes collects, per payload key, what a cast changes on a service
// without it reaching what is already running: the instances, or the volumes
// its claim templates made. It reads current state before anything is
// applied; a lookup that fails only loses the note.
func (c *Controller) serviceNotes(ctx context.Context, p Payloads) map[string][]string {
	notes := map[string][]string{}
	for key, rendered := range p.Services {
		if rendered == nil {
			continue
		}
		var n []string
		if stored, err := c.orch.GetService(ctx, rendered.Namespace, rendered.Name); err == nil && stored != nil {
			n = append(n, nextRollNotes(stored, rendered)...)
		}
		if hasClaimTemplate(rendered) {
			vols, err := c.volumes.List(ctx, rendered.Namespace)
			if err != nil {
				c.log.Debug("Cannot list volumes for cast notes",
					log.Str("namespace", rendered.Namespace), log.Err(err))
			}
			n = append(n, claimTemplateNotes(rendered, vols)...)
		}
		if len(n) > 0 {
			notes[key] = n
		}
	}
	return notes
}

// annotatePlan attaches the notes to the plan's changes for those services.
func annotatePlan(plan *release.Plan, notes map[string][]string) {
	if plan == nil {
		return
	}
	for i := range plan.Changes {
		if n, ok := notes[plan.Changes[i].Ref.Key()]; ok {
			plan.Changes[i].Notes = n
		}
	}
}

// nextRollNotes names the edited fields that are copied onto an instance
// when it is created, so running instances keep the old value until they
// are replaced. A template change replaces them anyway, so it needs no note.
func nextRollNotes(stored, rendered *types.Service) []string {
	if stored.Scale == 0 || rendered.Scale == 0 ||
		stored.CalculateTemplateHash() != rendered.CalculateTemplateHash() {
		return nil
	}
	var fields []string
	if !maps.Equal(stored.Labels, rendered.Labels) {
		fields = append(fields, "labels")
	}
	// The process runner pulls no image.
	if rendered.Runtime != types.RuntimeTypeProcess &&
		(effectiveImagePull(stored) != effectiveImagePull(rendered) ||
			stored.ImagePullAnonymous != rendered.ImagePullAnonymous) {
		fields = append(fields, "imagePull")
	}
	if len(fields) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s: running instances pick this up when they are next replaced (e.g. rune restart)",
		strings.Join(fields, ", "))}
}

// claimTemplateNotes compares each claim template with the volumes it has
// already made. A replica reattaches its existing volume by name, so those
// volumes keep what they were created with, whatever the template now says.
func claimTemplateNotes(svc *types.Service, vols []*types.Volume) []string {
	var notes []string
	for _, m := range svc.Volumes {
		if m.ClaimTemplate == nil {
			continue
		}
		existing := claimTemplateVolumes(svc, m.Name, vols)
		if len(existing) == 0 {
			continue
		}
		var sizes []string
		otherDiff := false
		for _, v := range existing {
			if v.Size != m.ClaimTemplate.Size && !slices.Contains(sizes, v.Size) {
				sizes = append(sizes, v.Size)
			}
			if v.StorageClassName != m.ClaimTemplate.StorageClassName || v.AccessMode != m.ClaimTemplate.AccessMode ||
				v.ReclaimPolicy != m.ClaimTemplate.ReclaimPolicy || !maps.Equal(v.Parameters, m.ClaimTemplate.Parameters) {
				otherDiff = true
			}
		}
		sort.Strings(sizes)
		switch {
		case len(sizes) > 0:
			notes = append(notes, fmt.Sprintf(
				"volume %s: existing volumes stay %s; only replicas without a volume yet get %s",
				m.Name, strings.Join(sizes, ", "), m.ClaimTemplate.Size))
		case otherDiff:
			notes = append(notes, fmt.Sprintf(
				"volume %s: the claim template change applies only to replicas without a volume yet", m.Name))
		}
	}
	return notes
}

// claimTemplateVolumes finds the per-replica volumes a claim template made
// for svc: named <mount>-<service>-<ordinal> and owned by the service.
func claimTemplateVolumes(svc *types.Service, mount string, vols []*types.Volume) []*types.Volume {
	prefix := mount + "-" + svc.Name + "-"
	owner := svc.Namespace + "/" + svc.Name
	var out []*types.Volume
	for _, v := range vols {
		if v == nil || v.OwnerService != owner || !strings.HasPrefix(v.Name, prefix) {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(v.Name, prefix)); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func hasClaimTemplate(s *types.Service) bool {
	for _, m := range s.Volumes {
		if m.ClaimTemplate != nil {
			return true
		}
	}
	return false
}

func effectiveImagePull(s *types.Service) string {
	if s.ImagePull == "" {
		return types.ImagePullAlways
	}
	return s.ImagePull
}
