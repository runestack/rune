package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// newReleaseCmd builds the `rune release` command group — the operator surface
// for stateful runeset releases (RUNESET_STATEFUL_RELEASES.md): list, get
// (status), history, diff, delete (uninstall), and rollback.
//
// The install/upgrade path (`rune cast --release X`) is intentionally NOT here;
// it lands with the cast UX refactor PR. This tree covers everything you do to
// an *existing* release.
func newReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "release",
		Aliases: []string{"releases"},
		Short:   "Manage stateful runeset releases (list, status, history, diff, delete, rollback)",
	}
	cmd.AddCommand(newReleaseListCmd())
	cmd.AddCommand(newReleaseGetCmd())
	cmd.AddCommand(newReleaseHistoryCmd())
	cmd.AddCommand(newReleaseDiffCmd())
	cmd.AddCommand(newReleaseDeleteCmd())
	cmd.AddCommand(newReleaseRollbackCmd())
	return cmd
}

func addReleaseNamespaceFlag(cmd *cobra.Command, ns *string) {
	cmd.Flags().StringVarP(ns, "namespace", "n", "default", "Namespace")
}

func addReleaseOutputFlag(cmd *cobra.Command, out *string) {
	cmd.Flags().StringVarP(out, "output", "o", "table", "Output format: table|json|yaml")
}

// --- list ---

func newReleaseListCmd() *cobra.Command {
	var ns, format string
	var allNamespaces, includeUninstalled bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List releases with revision, status, and resource counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			listNS := ns
			if allNamespaces {
				listNS = ""
			}
			rels, err := client.NewReleaseClient(api).ListReleases(listNS, includeUninstalled)
			if err != nil {
				return err
			}
			return renderReleaseList(rels, format)
		},
	}
	addReleaseNamespaceFlag(cmd, &ns)
	addReleaseOutputFlag(cmd, &format)
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List releases across all namespaces")
	cmd.Flags().BoolVar(&includeUninstalled, "all", false, "Include uninstalled (tombstone) releases")
	return cmd
}

// --- get (alias: status) ---

func newReleaseGetCmd() *cobra.Command {
	var ns, format string
	cmd := &cobra.Command{
		Use:     "get <name>",
		Aliases: []string{"status"},
		Short:   "Show a release: owned resources, revision, and status",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			rel, err := client.NewReleaseClient(api).GetRelease(ns, args[0])
			if err != nil {
				return err
			}
			return renderRelease(rel, format)
		},
	}
	addReleaseNamespaceFlag(cmd, &ns)
	addReleaseOutputFlag(cmd, &format)
	return cmd
}

// --- history ---

func newReleaseHistoryCmd() *cobra.Command {
	var ns, format string
	cmd := &cobra.Command{
		Use:   "history <name>",
		Short: "Show the revision log of a release",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			revs, err := client.NewReleaseClient(api).History(ns, args[0])
			if err != nil {
				return err
			}
			return renderReleaseHistory(revs, format)
		},
	}
	addReleaseNamespaceFlag(cmd, &ns)
	addReleaseOutputFlag(cmd, &format)
	return cmd
}

// --- diff (Plan / dry-run reconcile) ---

func newReleaseDiffCmd() *cobra.Command {
	var ns, format string
	cmd := &cobra.Command{
		Use:   "diff <name>",
		Short: "Dry-run reconcile: what a re-cast would create/update/prune",
		Long: `Diff computes the reconcile plan for a release without applying it.

It compares the release's currently-owned resource set against live state and
reports the create/update/prune/adopt/reference actions a re-cast would take.

Note: until the cast UX PR lands, diff plans against the release's *recorded*
owned set (the render-from-source path arrives with that PR), so for a steady
release it typically shows updates with no prunes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			rc := client.NewReleaseClient(api)
			rel, err := rc.GetRelease(ns, args[0])
			if err != nil {
				return err
			}
			plan, err := rc.Diff(ns, args[0], rel.Owns)
			if err != nil {
				return err
			}
			return renderReleasePlan(plan, format)
		},
	}
	addReleaseNamespaceFlag(cmd, &ns)
	addReleaseOutputFlag(cmd, &format)
	return cmd
}

// --- delete (alias: uninstall) ---

func newReleaseDeleteCmd() *cobra.Command {
	var ns string
	var keepVolumes, purge, yes bool
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"uninstall"},
		Short:   "Uninstall a release and its owned resources",
		Long: `Delete uninstalls a release: it removes every resource the release owns, in
reverse dependency order, then keeps a soft 'uninstalled' tombstone record by
default (Decision D4) so the release stays visible in 'release list --all' and
can be reinstalled or rolled back.

  --keep-volumes  retain volume data instead of reclaiming it
  --purge         remove the release record entirely (forget it)

Referenced shared kinds (StorageClass, Namespace) are never deleted (D2).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !yes {
				verb := "Uninstall"
				if purge {
					verb = "Uninstall and purge"
				}
				fmt.Fprintf(os.Stderr, "%s release %s/%s? Pass --yes to confirm.\n", verb, ns, name)
				return fmt.Errorf("aborted: confirmation required")
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			if err := client.NewReleaseClient(api).DeleteRelease(ns, name, keepVolumes, purge); err != nil {
				return err
			}
			if purge {
				fmt.Printf("Release %s/%s uninstalled and purged\n", ns, name)
			} else {
				fmt.Printf("Release %s/%s uninstalled (tombstone retained; use --purge to forget)\n", ns, name)
			}
			return nil
		},
	}
	addReleaseNamespaceFlag(cmd, &ns)
	cmd.Flags().BoolVar(&keepVolumes, "keep-volumes", false, "Retain volume data instead of reclaiming it")
	cmd.Flags().BoolVar(&purge, "purge", false, "Remove the release record entirely")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm destructive operation")
	return cmd
}

// --- rollback (wired to the server stub) ---

func newReleaseRollbackCmd() *cobra.Command {
	var ns string
	var revision int
	cmd := &cobra.Command{
		Use:   "rollback <name>",
		Short: "Roll a release forward to a prior revision",
		Long: `Rollback re-applies a prior revision's rendered set as a new revision
(forward-rolling, never mutating history).

Currently the server returns Unimplemented: rollback needs to re-render the
historical runeset source, which lands with the cast PR. The command is wired
so the surface exists and reports the deferral clearly.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			rel, err := client.NewReleaseClient(api).Rollback(ns, args[0], revision)
			if err != nil {
				return err
			}
			fmt.Printf("Release %s/%s rolled back (revision %d)\n", ns, args[0], rel.Revision)
			return nil
		},
	}
	addReleaseNamespaceFlag(cmd, &ns)
	cmd.Flags().IntVar(&revision, "to", 0, "Target revision to roll back to (0 = previous revision)")
	return cmd
}

// --- rendering ---

func renderReleaseList(rels []*types.Release, format string) error {
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(releaseListView(rels))
	case "yaml":
		return writeYAML(releaseListView(rels))
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAMESPACE\tNAME\tREVISION\tSTATUS\tRESOURCES\tUPDATED")
		for _, r := range rels {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%s\n",
				r.Namespace, r.Name, r.Revision, r.Status, len(r.Owns), formatTime(r.UpdatedAt))
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func renderRelease(rel *types.Release, format string) error {
	if rel == nil {
		return fmt.Errorf("nil release")
	}
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(releaseView(rel))
	case "yaml":
		return writeYAML(releaseView(rel))
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "Namespace:\t%s\n", rel.Namespace)
		fmt.Fprintf(w, "Name:\t%s\n", rel.Name)
		fmt.Fprintf(w, "Status:\t%s\n", rel.Status)
		fmt.Fprintf(w, "Revision:\t%d\n", rel.Revision)
		if rel.Source.Type != "" || rel.Source.Location != "" {
			fmt.Fprintf(w, "Source:\t%s %s\n", rel.Source.Type, rel.Source.Location)
		}
		fmt.Fprintf(w, "Created:\t%s\n", formatTime(rel.CreatedAt))
		fmt.Fprintf(w, "Updated:\t%s\n", formatTime(rel.UpdatedAt))
		fmt.Fprintln(w, "Owns:")
		for _, ref := range rel.Owns {
			fmt.Fprintf(w, "  %s\t%s/%s\n", ref.ResourceType, ref.Namespace, ref.Name)
		}
		if len(rel.References) > 0 {
			fmt.Fprintln(w, "References:")
			for _, ref := range rel.References {
				fmt.Fprintf(w, "  %s\t%s/%s\n", ref.ResourceType, ref.Namespace, ref.Name)
			}
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func renderReleaseHistory(revs []*types.Release, format string) error {
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(releaseListView(revs))
	case "yaml":
		return writeYAML(releaseListView(revs))
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "REVISION\tSTATUS\tRESOURCES\tUPDATED")
		for _, r := range revs {
			fmt.Fprintf(w, "%d\t%s\t%d\t%s\n", r.Revision, r.Status, len(r.Owns), formatTime(r.UpdatedAt))
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func renderReleasePlan(plan *client.Plan, format string) error {
	if plan == nil {
		return fmt.Errorf("nil plan")
	}
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(plan)
	case "yaml":
		return writeYAML(plan)
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ACTION\tRESOURCE\tCONFLICT")
		for _, ch := range plan.Changes {
			ref := fmt.Sprintf("%s/%s/%s", ch.ResourceType, ch.Namespace, ch.Name)
			fmt.Fprintf(w, "%s\t%s\t%s\n", ch.Action, ref, ch.Conflict)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if !plan.Applyable {
			fmt.Fprintln(os.Stderr, "\nPlan has conflicts; pass --adopt on cast to take ownership.")
		}
		return nil
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func releaseView(rel *types.Release) map[string]interface{} {
	return map[string]interface{}{
		"namespace":  rel.Namespace,
		"name":       rel.Name,
		"status":     rel.Status,
		"revision":   rel.Revision,
		"source":     rel.Source,
		"owns":       rel.Owns,
		"references": rel.References,
		"createdAt":  rel.CreatedAt,
		"updatedAt":  rel.UpdatedAt,
	}
}

func releaseListView(rels []*types.Release) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rels))
	for _, r := range rels {
		out = append(out, releaseView(r))
	}
	return out
}
