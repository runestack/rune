// Package cmd — `rune snapshot` noun-tree subcommands.
//
// Mirrors the `rune volume` precedent at pkg/cli/cmd/volume.go. The
// underlying gRPC SnapshotService is namespace-scoped, so all
// subcommands accept --namespace (default: "default"). list adds
// --all-namespaces.
//
// Wired against the live SnapshotService in RUNE-071 (Slice 10a).
package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// newSnapshotCmd builds the `rune snapshot` command group.
func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "snapshot",
		Aliases: []string{"snap", "snapshots"},
		Short:   "Manage volume snapshots (list, get, create, delete, restore)",
	}
	cmd.AddCommand(newSnapshotListCmd())
	cmd.AddCommand(newSnapshotGetCmd())
	cmd.AddCommand(newSnapshotCreateCmd())
	cmd.AddCommand(newSnapshotDeleteCmd())
	cmd.AddCommand(newSnapshotRestoreCmd())
	return cmd
}

func newSnapshotListCmd() *cobra.Command {
	var ns, format, labelSelector string
	var allNamespaces bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List snapshots in a namespace",
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			labels := parseLabelSelectorString(labelSelector)
			target := ns
			if allNamespaces {
				target = "*"
			}
			snaps, err := client.NewSnapshotClient(api).ListSnapshots(target, labels)
			if err != nil {
				return err
			}
			sort.Slice(snaps, func(i, j int) bool {
				if snaps[i].Namespace != snaps[j].Namespace {
					return snaps[i].Namespace < snaps[j].Namespace
				}
				return snaps[i].Name < snaps[j].Name
			})
			return renderSnapshots(snaps, format, allNamespaces)
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List snapshots across all namespaces")
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table|json|yaml|name")
	cmd.Flags().StringVarP(&labelSelector, "selector", "l", "", "Label selector (key=value,key=value)")
	return cmd
}

func newSnapshotGetCmd() *cobra.Command {
	var ns, format string
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a snapshot's full status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			s, err := client.NewSnapshotClient(api).GetSnapshot(ns, args[0])
			if err != nil {
				return err
			}
			return renderSnapshot(s, format)
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table|json|yaml")
	return cmd
}

func newSnapshotCreateCmd() *cobra.Command {
	var ns, name string
	var ensureNamespace bool
	cmd := &cobra.Command{
		Use:   "create <volume>",
		Short: "Create a snapshot of an existing volume",
		Long: `Captures a point-in-time snapshot of <volume>. The new Snapshot row
starts in Pending; the snapshot controller drives it through
Creating -> Ready by calling Driver.Snapshot.

If --name is not given, a generated name "<volume>-snap-<unix>" is used.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			source := args[0]
			snapName := name
			if snapName == "" {
				snapName = fmt.Sprintf("%s-snap-%d", source, time.Now().Unix())
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			snap := &types.Snapshot{
				Name:         snapName,
				Namespace:    ns,
				SourceVolume: source,
			}
			created, err := client.NewSnapshotClient(api).CreateSnapshot(snap, ensureNamespace)
			if err != nil {
				return err
			}
			fmt.Printf("Snapshot %s/%s created (source=%s, phase=%s)\n",
				created.Namespace, created.Name, created.SourceVolume, created.Phase)
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringVar(&name, "name", "", "Snapshot name (default: <volume>-snap-<unix>)")
	cmd.Flags().BoolVar(&ensureNamespace, "ensure-namespace", false, "Auto-create the target namespace if missing")
	return cmd
}

func newSnapshotDeleteCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"remove", "rm"},
		Short:   "Delete a snapshot by name",
		Long: `Marks the named Snapshot for deletion. The snapshot controller then
calls Driver.DeleteSnapshot and removes the row.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			if err := client.NewSnapshotClient(api).DeleteSnapshot(ns, args[0]); err != nil {
				return err
			}
			fmt.Printf("Snapshot %s/%s deleted\n", ns, args[0])
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	return cmd
}

func newSnapshotRestoreCmd() *cobra.Command {
	var ns, asNew, targetNS, scName string
	cmd := &cobra.Command{
		Use:   "restore <snapshot> --as <new-volume>",
		Short: "Provision a new volume from a snapshot",
		Long: `Restore creates a new volume named <new-volume> populated from the
named snapshot's handle. The snapshot must be in phase Ready.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			if asNew == "" {
				return fmt.Errorf("--as is required (target volume name)")
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			vol, err := client.NewSnapshotClient(api).RestoreVolume(
				ns, args[0], asNew, targetNS, scName, nil)
			if err != nil {
				return err
			}
			fmt.Printf("Volume %s/%s created from snapshot %s/%s (status=%s)\n",
				vol.Namespace, vol.Name, ns, args[0], vol.Status)
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Source snapshot namespace")
	cmd.Flags().StringVar(&asNew, "as", "", "Name for the new volume provisioned from the snapshot")
	cmd.Flags().StringVar(&targetNS, "target-namespace", "", "Namespace for the new volume (default: source snapshot namespace)")
	cmd.Flags().StringVar(&scName, "storage-class", "", "Override storage class for the new volume (default: source volume's class)")
	return cmd
}

// --- rendering helpers ---

func renderSnapshots(snaps []*types.Snapshot, format string, allNamespaces bool) error {
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(snaps)
	case "yaml":
		return writeYAML(snaps)
	case "name":
		for _, s := range snaps {
			if allNamespaces {
				fmt.Printf("%s/%s\n", s.Namespace, s.Name)
			} else {
				fmt.Println(s.Name)
			}
		}
		return nil
	case "", "table":
		if len(snaps) == 0 {
			fmt.Println("No snapshots found")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		if allNamespaces {
			fmt.Fprintln(w, "NAMESPACE\tNAME\tSOURCE\tPHASE\tDRIVER\tSIZE\tAGE")
		} else {
			fmt.Fprintln(w, "NAME\tSOURCE\tPHASE\tDRIVER\tSIZE\tAGE")
		}
		for _, s := range snaps {
			drv := s.Driver
			if drv == "" {
				drv = "-"
			}
			size := "-"
			if s.SizeBytes > 0 {
				size = fmt.Sprintf("%d", s.SizeBytes)
			}
			if allNamespaces {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					s.Namespace, s.Name, s.SourceVolume, s.Phase, drv, size, formatAgeTable(s.CreatedAt))
			} else {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					s.Name, s.SourceVolume, s.Phase, drv, size, formatAgeTable(s.CreatedAt))
			}
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func renderSnapshot(s *types.Snapshot, format string) error {
	if s == nil {
		return fmt.Errorf("nil snapshot")
	}
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(s)
	case "yaml":
		return writeYAML(s)
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "Name:\t%s\n", s.Name)
		fmt.Fprintf(w, "Namespace:\t%s\n", s.Namespace)
		fmt.Fprintf(w, "SourceVolume:\t%s\n", s.SourceVolume)
		fmt.Fprintf(w, "Driver:\t%s\n", s.Driver)
		fmt.Fprintf(w, "Handle:\t%s\n", s.Handle)
		fmt.Fprintf(w, "SizeBytes:\t%d\n", s.SizeBytes)
		fmt.Fprintf(w, "Phase:\t%s\n", s.Phase)
		if s.Reason != "" {
			fmt.Fprintf(w, "Reason:\t%s\n", s.Reason)
		}
		if s.Message != "" {
			fmt.Fprintf(w, "Message:\t%s\n", s.Message)
		}
		fmt.Fprintf(w, "CreatedAt:\t%s\n", s.CreatedAt)
		fmt.Fprintf(w, "UpdatedAt:\t%s\n", s.UpdatedAt)
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}
