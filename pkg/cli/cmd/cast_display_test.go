package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/stretchr/testify/require"
)

func planWith(changes ...client.PlannedChange) *client.Plan {
	p := &client.Plan{Release: "app", Namespace: "default", Applyable: true}
	for _, c := range changes {
		if c.Conflict != "" {
			p.Applyable = false
		}
		p.Changes = append(p.Changes, c)
	}
	return p
}

func ch(action, rt, name string) client.PlannedChange {
	return client.PlannedChange{Action: action, ResourceType: rt, Namespace: "default", Name: name}
}

func TestPlanHasPrune(t *testing.T) {
	require.False(t, planHasPrune(planWith(ch("create", "service", "web"))))
	require.True(t, planHasPrune(planWith(ch("create", "service", "web"), ch("prune", "configmap", "old"))))
}

func TestPlanHasConfirmable(t *testing.T) {
	// Pure create/update needs no confirm.
	require.False(t, planHasConfirmable(planWith(ch("create", "service", "web"), ch("update", "service", "api"))))
	// Prune or adopt requires confirm.
	require.True(t, planHasConfirmable(planWith(ch("prune", "configmap", "old"))))
	require.True(t, planHasConfirmable(planWith(ch("adopt", "secret", "creds"))))
}

func TestConfirmApply_AutoSkips(t *testing.T) {
	// --yes always skips.
	ok, err := confirmApply(planWith(ch("prune", "configmap", "old")), &castOptions{yes: true})
	require.NoError(t, err)
	require.True(t, ok)

	// detach implicitly skips (and is separately guarded against prune).
	ok, err = confirmApply(planWith(ch("create", "service", "web")), &castOptions{detach: true})
	require.NoError(t, err)
	require.True(t, ok)

	// Pure create/update needs no confirm even without --yes.
	ok, err = confirmApply(planWith(ch("create", "service", "web")), &castOptions{})
	require.NoError(t, err)
	require.True(t, ok)
}

func TestRenderPlanBlock_FlagsPruneDestructive(t *testing.T) {
	var buf bytes.Buffer
	plan := planWith(
		ch("create", "service", "query-api"),
		ch("update", "service", "ingester"),
		ch("prune", "configmap", "old-settings"),
	)
	destructive := renderPlanBlock(&buf, "runesight", "observability", 3, plan)
	out := buf.String()

	require.True(t, destructive, "a plan with a prune must report destructive")
	require.Contains(t, out, "Release")
	require.Contains(t, out, "runesight")
	require.Contains(t, out, "revision 3")
	require.Contains(t, out, "create")
	require.Contains(t, out, "update")
	require.Contains(t, out, "prune")
	require.Contains(t, out, "destructive")
}

func TestRenderPlanBlock_NoPrune_NotDestructive(t *testing.T) {
	var buf bytes.Buffer
	plan := planWith(ch("create", "service", "web"))
	destructive := renderPlanBlock(&buf, "app", "default", 1, plan)
	require.False(t, destructive)
}

func TestRenderPlanBlock_SurfacesConflicts(t *testing.T) {
	var buf bytes.Buffer
	conflict := ch("adopt", "secret", "creds")
	conflict.Conflict = "resource is owned by release \"other\"; pass --adopt to take ownership"
	plan := planWith(conflict)
	renderPlanBlock(&buf, "app", "default", 2, plan)
	out := buf.String()
	require.Contains(t, out, "conflict")
	require.Contains(t, out, "--adopt")
}

func TestBuildCastJSON_CountsAndResult(t *testing.T) {
	plan := planWith(
		ch("create", "service", "web"),
		ch("create", "volume", "data"),
		ch("prune", "configmap", "old"),
	)
	res := &castReleaseResult{
		Revision: 4,
		Status:   "deployed",
		Owns:     []castJSONResource{{ResourceType: "service", Namespace: "default", Name: "web"}},
	}
	out := buildCastJSON("app", "default", false, plan, res)
	require.Equal(t, "app", out.Release)
	require.Equal(t, 2, out.Counts["create"])
	require.Equal(t, 1, out.Counts["prune"])
	require.Equal(t, 4, out.Revision)
	require.Equal(t, "deployed", out.Status)
	require.Len(t, out.Owns, 1)
	require.False(t, out.DryRun)
}

func TestStorageClassDisplaySuffix(t *testing.T) {
	var buf bytes.Buffer
	plan := planWith(ch("reference", "storage_class", "fast"))
	renderPlanBlock(&buf, "app", "default", 1, plan)
	// storage_class renders as "storageclass" in the plan block.
	require.Contains(t, strings.ToLower(buf.String()), "storageclass/fast")
}
