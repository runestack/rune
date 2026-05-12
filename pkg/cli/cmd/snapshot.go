// Package cmd — `rune snapshot` noun-tree subcommand stubs.
//
// The SnapshotService is not yet implemented (RUNE-071); these stubs exist
// so the command shape is documented in --help and so muscle-memory invocations
// produce a clear "not yet implemented" error rather than "unknown command".
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newSnapshotCmd builds the `rune snapshot` command group.
func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "snapshot",
		Aliases: []string{"snap", "snapshots"},
		Short:   "Manage volume snapshots (not yet implemented — RUNE-071)",
	}
	cmd.AddCommand(newSnapshotListCmd())
	cmd.AddCommand(newSnapshotGetCmd())
	cmd.AddCommand(newSnapshotCreateCmd())
	cmd.AddCommand(newSnapshotDeleteCmd())
	cmd.AddCommand(newSnapshotRestoreCmd())
	return cmd
}

func newSnapshotListCmd() *cobra.Command {
	var ns string
	var allNamespaces bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List snapshots in a namespace (not yet implemented)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = ns
			_ = allNamespaces
			return fmt.Errorf("rune snapshot list: not yet implemented (RUNE-071)")
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List snapshots across all namespaces")
	return cmd
}

func newSnapshotGetCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a snapshot's status (not yet implemented)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = ns
			return fmt.Errorf("rune snapshot get: not yet implemented (snapshot %q, RUNE-071)", args[0])
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	return cmd
}

func newSnapshotCreateCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:   "create <volume>",
		Short: "Create a snapshot of an existing volume (not yet implemented)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = ns
			return fmt.Errorf("rune snapshot create: not yet implemented (volume %q, RUNE-071)", args[0])
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	return cmd
}

func newSnapshotDeleteCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"remove", "rm"},
		Short:   "Delete a snapshot by name (not yet implemented)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = ns
			return fmt.Errorf("rune snapshot delete: not yet implemented (snapshot %q, RUNE-071)", args[0])
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	return cmd
}

func newSnapshotRestoreCmd() *cobra.Command {
	var ns, asNew string
	cmd := &cobra.Command{
		Use:   "restore <snapshot> --as <new-volume>",
		Short: "Provision a new volume from a snapshot (alias of `rune volume restore`)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = ns
			if asNew == "" {
				return fmt.Errorf("--as is required (target volume name)")
			}
			return fmt.Errorf("rune snapshot restore: not yet implemented (snapshot %q -> %q, RUNE-071)", args[0], asNew)
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringVar(&asNew, "as", "", "Name for the new volume provisioned from the snapshot")
	return cmd
}
