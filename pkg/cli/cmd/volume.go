// Package cmd — `rune volume` noun-tree subcommands.
//
// Mirrors the `rune secret` precedent at pkg/cli/cmd/secret.go. The
// underlying gRPC service is namespace-scoped, so all subcommands accept
// --namespace (default: "default"). list adds --all-namespaces.
//
// detach and retry-provision call the VolumeService RPCs of the same
// name. restore is still stubbed pending the SnapshotService (RUNE-071).
package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// newVolumeCmd builds the `rune volume` command group.
func newVolumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "volume",
		Aliases: []string{"vol", "volumes"},
		Short:   "Manage persistent volumes (list, get, delete, detach, retry-provision, restore)",
		Long: `Inspect and manage Volume resources. To create a Volume from a YAML
spec, use ` + "`rune cast`" + ` — the same path used for every other resource
(services, secrets, configmaps, storageclasses). Example:

    rune cast my-volume.yaml

A Volume cast file is a top-level ` + "`volume:`" + ` mapping; see
https://docs.runestack.io/reference/storage-resources/#volume.`,
	}
	cmd.AddCommand(newVolumeListCmd())
	cmd.AddCommand(newVolumeGetCmd())
	cmd.AddCommand(newVolumeDeleteCmd())
	cmd.AddCommand(newVolumeDetachCmd())
	cmd.AddCommand(newVolumeRetryProvisionCmd())
	cmd.AddCommand(newVolumeRestoreCmd())
	return cmd
}

// --- list ---

func newVolumeListCmd() *cobra.Command {
	var ns, format, labelSelector, fieldSelector string
	var allNamespaces bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List volumes in a namespace",
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			labels := parseLabelSelectorString(labelSelector)
			fields := parseLabelSelectorString(fieldSelector)
			target := ns
			if allNamespaces {
				target = "*"
			}
			vols, err := client.NewVolumeClient(api).ListVolumes(target, labels, fields)
			if err != nil {
				return err
			}
			sort.Slice(vols, func(i, j int) bool {
				if vols[i].Namespace != vols[j].Namespace {
					return vols[i].Namespace < vols[j].Namespace
				}
				return vols[i].Name < vols[j].Name
			})
			return renderVolumes(vols, format, allNamespaces)
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List volumes across all namespaces")
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table|json|yaml|name")
	cmd.Flags().StringVarP(&labelSelector, "selector", "l", "", "Label selector (key=value,key=value)")
	cmd.Flags().StringVar(&fieldSelector, "field-selector", "", "Field selector (key=value,key=value)")
	return cmd
}

// --- get ---

func newVolumeGetCmd() *cobra.Command {
	var ns, format string
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a volume's full status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			v, err := client.NewVolumeClient(api).GetVolume(ns, args[0])
			if err != nil {
				return err
			}
			return renderVolume(v, format)
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table|json|yaml")
	return cmd
}

// --- delete ---

func newVolumeDeleteCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"remove", "rm"},
		Short:   "Delete a volume by name",
		Long: `Delete the named Volume row. The driver's reclaimPolicy then determines
whether the underlying storage is destroyed (delete) or left in place
(retain). Operator-owned volumes (no OwnerService) follow the row's own
reclaimPolicy.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			if err := client.NewVolumeClient(api).DeleteVolume(ns, args[0]); err != nil {
				return err
			}
			fmt.Printf("Volume %s/%s deleted\n", ns, args[0])
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	return cmd
}

// --- detach ---

func newVolumeDetachCmd() *cobra.Command {
	var ns string
	var force bool
	cmd := &cobra.Command{
		Use:   "detach <name>",
		Short: "Detach a Bound volume so a replacement instance can attach it",
		Long: `Detach clears bind state (BoundClaim/BoundNode) on a volume so a
replacement instance may attach it. Without --force the server
refuses to disturb a Bound volume; pass --force to override (caller
assumes responsibility for any data-loss risk if the previous holder
is still alive).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			v, err := client.NewVolumeClient(api).DetachVolume(ns, args[0], force)
			if err != nil {
				return err
			}
			fmt.Printf("Volume %s/%s detached (status=%s)\n", v.Namespace, v.Name, v.Status)
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().BoolVar(&force, "force", false, "Force detach even if the volume is currently Bound (data-loss risk)")
	return cmd
}

// --- retry-provision ---

func newVolumeRetryProvisionCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:   "retry-provision <name>",
		Short: "Retry provisioning a Failed/Stalled volume",
		Long: `Retry-provision resets a Failed or Stalled volume back to Pending so
the controller will attempt provisioning again on its next watch
event. Volumes in any other state are rejected.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns = effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			v, err := client.NewVolumeClient(api).RetryProvisionVolume(ns, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Volume %s/%s retry queued (status=%s)\n", v.Namespace, v.Name, v.Status)
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	return cmd
}

// --- restore ---

func newVolumeRestoreCmd() *cobra.Command {
	var ns, fromSnapshot, snapshotNS, scName string
	cmd := &cobra.Command{
		Use:   "restore <name> --from-snapshot <snap>",
		Short: "Provision a new volume from a snapshot",
		Long: `Restore creates a new volume named <name> populated from the named
snapshot's handle. The snapshot must be in phase Ready. The new volume
is created in the same namespace as the snapshot unless -n/--namespace
is given.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromSnapshot == "" {
				return fmt.Errorf("--from-snapshot is required")
			}
			snapNS := snapshotNS
			if snapNS == "" {
				snapNS = effectiveCmdNS(ns)
			}
			targetNS := effectiveCmdNS(ns)
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			vol, err := client.NewSnapshotClient(api).RestoreVolume(
				snapNS, fromSnapshot, args[0], targetNS, scName, nil)
			if err != nil {
				return err
			}
			fmt.Printf("Volume %s/%s created from snapshot %s/%s (status=%s)\n",
				vol.Namespace, vol.Name, snapNS, fromSnapshot, vol.Status)
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace for the new volume (and source snapshot, unless --snapshot-namespace is set)")
	cmd.Flags().StringVar(&fromSnapshot, "from-snapshot", "", "Source snapshot name (required)")
	cmd.Flags().StringVar(&snapshotNS, "snapshot-namespace", "", "Source snapshot namespace (default: --namespace)")
	cmd.Flags().StringVar(&scName, "storage-class", "", "Override storage class for the new volume (default: source volume's class)")
	return cmd
}

// --- rendering helpers ---

func renderVolumes(vols []*types.Volume, format string, allNamespaces bool) error {
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(vols)
	case "yaml":
		return writeYAML(vols)
	case "name":
		for _, v := range vols {
			if allNamespaces {
				fmt.Printf("%s/%s\n", v.Namespace, v.Name)
			} else {
				fmt.Println(v.Name)
			}
		}
		return nil
	case "", "table":
		if len(vols) == 0 {
			fmt.Println("No volumes found")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		if allNamespaces {
			fmt.Fprintln(w, "NAMESPACE\tNAME\tSTATUS\tCLASS\tSIZE\tACCESS\tBOUND\tAGE")
		} else {
			fmt.Fprintln(w, "NAME\tSTATUS\tCLASS\tSIZE\tACCESS\tBOUND\tAGE")
		}
		for _, v := range vols {
			bound := "-"
			if v.BoundClaim != "" {
				bound = v.BoundClaim
			}
			access := string(v.AccessMode)
			if access == "" {
				access = "-"
			}
			size := v.Size
			if size == "" {
				size = "-"
			}
			if allNamespaces {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					v.Namespace, v.Name, v.Status, v.StorageClassName, size, access, bound, formatAgeTable(v.CreatedAt))
			} else {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					v.Name, v.Status, v.StorageClassName, size, access, bound, formatAgeTable(v.CreatedAt))
			}
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func renderVolume(v *types.Volume, format string) error {
	if v == nil {
		return fmt.Errorf("nil volume")
	}
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(v)
	case "yaml":
		return writeYAML(v)
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "Namespace:\t%s\n", v.Namespace)
		fmt.Fprintf(w, "Name:\t%s\n", v.Name)
		fmt.Fprintf(w, "StorageClass:\t%s\n", v.StorageClassName)
		fmt.Fprintf(w, "Size:\t%s\n", emptyDash(v.Size))
		fmt.Fprintf(w, "AccessMode:\t%s\n", emptyDash(string(v.AccessMode)))
		fmt.Fprintf(w, "ReclaimPolicy:\t%s\n", emptyDash(string(v.ReclaimPolicy)))
		fmt.Fprintf(w, "Status:\t%s\n", v.Status)
		if v.Reason != "" {
			fmt.Fprintf(w, "Reason:\t%s\n", v.Reason)
		}
		if v.Message != "" {
			fmt.Fprintf(w, "Message:\t%s\n", v.Message)
		}
		fmt.Fprintf(w, "Handle:\t%s\n", emptyDash(v.Handle))
		fmt.Fprintf(w, "BoundNode:\t%s\n", emptyDash(v.BoundNode))
		fmt.Fprintf(w, "BoundClaim:\t%s\n", emptyDash(v.BoundClaim))
		if v.OwnerService != "" {
			fmt.Fprintf(w, "OwnerService:\t%s\n", v.OwnerService)
		}
		fmt.Fprintf(w, "Created:\t%s\n", formatTime(v.CreatedAt))
		fmt.Fprintf(w, "Updated:\t%s\n", formatTime(v.UpdatedAt))
		if len(v.Parameters) > 0 {
			fmt.Fprintln(w, "Parameters:")
			for _, k := range sortedKeys(v.Parameters) {
				fmt.Fprintf(w, "  %s:\t%s\n", k, v.Parameters[k])
			}
		}
		if len(v.Labels) > 0 {
			fmt.Fprintln(w, "Labels:")
			for _, k := range sortedKeys(v.Labels) {
				fmt.Fprintf(w, "  %s:\t%s\n", k, v.Labels[k])
			}
		}
		if v.SnapshotSchedule != nil {
			fmt.Fprintf(w, "SnapshotSchedule:\t%s (retention: %d)\n",
				v.SnapshotSchedule.Cron, v.SnapshotSchedule.Retention)
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}
