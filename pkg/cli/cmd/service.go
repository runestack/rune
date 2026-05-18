package cmd

import (
	"github.com/spf13/cobra"
)

// newServiceCmd is the noun-first entry point for service operations,
// matching the shape of `rune secret`, `rune storageclass`, etc. The
// catch-all `rune delete <name>` shorthand stays — this is a clearer
// path when the user already knows the resource type. Operators reach
// for `rune service delete prod-gateway` more naturally than
// `rune delete service prod-gateway`, and a typo there used to be
// silently misinterpreted as a different resource type.
//
// New subcommands should be added here as the noun-first surface
// expands; we deliberately do NOT auto-mirror every verb under
// `rune service` to keep this incremental and avoid double-maintenance.
func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage services (delete)",
		Long: `Manage services in the noun-first style (rune service <verb>).

This currently exposes 'delete' as the operator-facing entry point;
other verbs (get, list, scale, restart, stop) remain available under
their existing top-level commands (rune get services, rune scale, ...)
until they are migrated here.`,
	}
	cmd.AddCommand(newServiceDeleteCmd())
	return cmd
}

// newServiceDeleteCmd is the `rune service delete <name>` subcommand.
// It shares the exact same flags, options, and runtime behaviour as
// `rune delete service <name>` — both call into runServiceDelete.
// Keeping a thin wrapper rather than aliasing the cobra.Command avoids
// the awkward `Use:` string that would otherwise read as `service`
// under `rune service delete`.
func newServiceDeleteCmd() *cobra.Command {
	opts := &deleteOptions{}

	cmd := &cobra.Command{
		Use:   "delete <service-name>",
		Short: "Delete a service and its resources",
		Long: `Delete a service and all its associated resources.

Equivalent to 'rune delete service <name>' — same flags, same behaviour.
Provided so operators can stay in the noun-first style (rune service ...)
when they already know they are operating on a service.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runServiceDelete(cmd.Context(), args[0], opts)
		},
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace of the service")
	cmd.Flags().BoolVarP(&opts.force, "force", "f", false, "Skip confirmation prompt")
	cmd.Flags().Int32VarP(&opts.timeoutSeconds, "timeout", "t", 30, "Graceful shutdown timeout in seconds")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start deletion and return immediately")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Show what would be deleted without actually deleting")
	cmd.Flags().Int32Var(&opts.gracePeriod, "grace-period", 0, "Grace period for graceful shutdown (alternative to --timeout)")
	cmd.Flags().BoolVar(&opts.now, "now", false, "Immediate deletion without graceful shutdown")
	cmd.Flags().BoolVar(&opts.ignoreNotFound, "ignore-not-found", false, "Don't error if service doesn't exist")
	cmd.Flags().StringSliceVar(&opts.finalizers, "finalizers", nil, "Optional finalizers to run")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "Output format (text, json, yaml)")
	cmd.Flags().BoolVar(&opts.noDependencies, "no-dependencies", false, "Ignore dependents and proceed with deletion")

	cmd.MarkFlagsMutuallyExclusive("detach", "now")
	cmd.MarkFlagsMutuallyExclusive("timeout", "grace-period")

	return cmd
}
