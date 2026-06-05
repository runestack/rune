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

// newConfigmapCmd builds the `rune configmap` command group — the configmap
// counterpart to `rune secret`, at parity except for `reveal` (configmaps are
// plaintext, so `get` already shows the data). get/list/update/set/unset/
// versions/rollback/delete all mirror the secret group.
//
// The historical `rune get config` / `rune create config` / `rune delete
// config` commands continue to work and share the same gRPC plumbing.
func newConfigmapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "configmap",
		Aliases: []string{"configmaps"},
		Short:   "Manage configmaps (get, list, update, set, unset, versions, rollback, delete)",
	}
	cmd.AddCommand(newConfigmapGetCmd())
	cmd.AddCommand(newConfigmapListCmd())
	cmd.AddCommand(newConfigmapUpdateCmd())
	cmd.AddCommand(newConfigmapSetCmd())
	cmd.AddCommand(newConfigmapUnsetCmd())
	cmd.AddCommand(newConfigmapVersionsCmd())
	cmd.AddCommand(newConfigmapRollbackCmd())
	cmd.AddCommand(newConfigmapDeleteCmd())
	return cmd
}

// --- get (alias for `rune get config <name>`) ---

func newConfigmapGetCmd() *cobra.Command {
	opts := &getOptions{outputFormat: "table"}
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a configmap (including its data)",
		Long: `Get returns a configmap — namespace, name, version, timestamps and data.
Configmaps are plaintext, so the data map is shown directly (unlike secrets,
which require 'rune secret reveal').

Alias for 'rune get config <name>' — both share the same handler and output
formatting.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleConfigmapGet(cmd, opts, args[0])
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", "table", "Output format: table|json|yaml")
	return cmd
}

// --- list (alias for `rune get configmaps`) ---

func newConfigmapListCmd() *cobra.Command {
	opts := &getOptions{outputFormat: "table"}
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configmaps in a namespace",
		Long: `Alias for 'rune get configmaps' — both share the same handler, label/field
selectors, and output formatting.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// resourceName == "" tells handleConfigmapGet to list rather than get-one.
			return handleConfigmapGet(cmd, opts, "")
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace")
	cmd.Flags().BoolVarP(&opts.allNamespaces, "all-namespaces", "A", false, "List configmaps across all namespaces")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", "table", "Output format: table|json|yaml|wide|name")
	cmd.Flags().StringVarP(&opts.labelSelector, "selector", "l", "", "Label selector (key=value,key=value)")
	cmd.Flags().StringVar(&opts.fieldSelector, "field-selector", "", "Field selector (key=value)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "Maximum number of configmaps to return (0 = unlimited)")
	return cmd
}

// --- update (replace the whole data map) ---

func newConfigmapUpdateCmd() *cobra.Command {
	var ns string
	var dataPairs []string
	var fromFile []string
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update an existing configmap's data (replaces the data map)",
		Long: `Update rewrites the configmap's data map and bumps its version. Provide the
full desired data with --data and/or --from-file. To change individual keys
without replacing the rest, use 'rune configmap set' / 'unset'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			data := map[string]string{}
			if err := applyFromFileFlags(fromFile, data); err != nil {
				return err
			}
			for _, pair := range dataPairs {
				k, v, err := splitPair(pair)
				if err != nil {
					return err
				}
				data[k] = v
			}
			if len(data) == 0 {
				return fmt.Errorf("no data provided. Use --data flags or --from-file")
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			cfg := &types.Configmap{Name: name, Namespace: ns, Data: data}
			if err := client.NewConfigmapClient(api).UpdateConfigmap(cfg); err != nil {
				return err
			}
			fmt.Printf("Configmap %s/%s updated with %d data entries\n", ns, name, len(data))
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringArrayVar(&dataPairs, "data", nil, "Data entry key=value (can repeat; value is taken verbatim — no comma/newline splitting)")
	cmd.Flags().StringArrayVar(&fromFile, "from-file", nil, "Read data from file: --from-file=key=path (file's bytes become the value for key — use for multi-line content). Can repeat.")
	return cmd
}

// --- set (server-side merge) ---

func newConfigmapSetCmd() *cobra.Command {
	var ns string
	var fromFile []string
	cmd := &cobra.Command{
		Use:   "set <name> [KEY=VALUE ...]",
		Short: "Upsert one or more keys in a configmap (server-side merge)",
		Long: `Set upserts the given keys into an existing configmap's data map without
touching the other keys. The server performs the merge atomically under a
per-configmap lock and writes a new version.

Examples:
  rune configmap set app-config -n prod LOG_LEVEL=debug
  rune configmap set app-config -n prod KEY1=v1 KEY2=v2
  rune configmap set app-config -n prod --from-file=nginx.conf=./nginx.conf`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			set := map[string]string{}
			if err := applyFromFileFlags(fromFile, set); err != nil {
				return err
			}
			for _, pair := range args[1:] {
				k, v, err := splitPair(pair)
				if err != nil {
					return err
				}
				set[k] = v
			}
			if len(set) == 0 {
				return fmt.Errorf("no keys provided. Pass KEY=VALUE positional args and/or --from-file")
			}

			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			out, err := client.NewConfigmapClient(api).PatchConfigmap(ns, name, set, nil)
			if err != nil {
				return err
			}
			keys := keysOf(set)
			sort.Strings(keys)
			fmt.Printf("Configmap %s/%s patched (v%d): set %s\n", ns, name, out.Version, strings.Join(keys, ","))
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringArrayVar(&fromFile, "from-file", nil, "Read a key from a file: --from-file=key=path (file bytes become the value). Can repeat.")
	return cmd
}

// --- unset (server-side merge, idempotent) ---

func newConfigmapUnsetCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:   "unset <name> KEY [KEY ...]",
		Short: "Remove one or more keys from a configmap (server-side merge)",
		Long: `Unset removes the listed keys from an existing configmap's data map without
touching the other keys. Missing keys are silently ignored — re-running the
same unset is safe. The server performs the merge atomically.

Examples:
  rune configmap unset app-config -n prod LEGACY_FLAG
  rune configmap unset app-config -n prod KEY1 KEY2`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			unset := args[1:]

			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			out, err := client.NewConfigmapClient(api).PatchConfigmap(ns, name, nil, unset)
			if err != nil {
				return err
			}
			sort.Strings(unset)
			fmt.Printf("Configmap %s/%s patched (v%d): unset %s\n", ns, name, out.Version, strings.Join(unset, ","))
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	return cmd
}

// --- versions ---

func newConfigmapVersionsCmd() *cobra.Command {
	var ns, format string
	cmd := &cobra.Command{
		Use:   "versions <name>",
		Short: "List historical versions of a configmap",
		Long: `Versions returns metadata for every historical version of a configmap,
newest first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			versions, err := client.NewConfigmapClient(api).ListConfigmapVersions(ns, args[0])
			if err != nil {
				return err
			}
			return renderConfigmapVersions(versions, format)
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table|json|yaml")
	return cmd
}

// --- rollback ---

func newConfigmapRollbackCmd() *cobra.Command {
	var ns string
	var toVersion int
	var yes bool
	cmd := &cobra.Command{
		Use:   "rollback <name>",
		Short: "Rewrite a configmap's HEAD to the contents of a prior version",
		Long: `Rollback fetches the named historical version and writes its data as a new
HEAD version (head+1). Old versions are retained — rollback never deletes
history.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if toVersion <= 0 {
				return fmt.Errorf("--to is required and must be > 0")
			}
			if !yes {
				fmt.Fprintf(os.Stderr, "Rollback configmap %s/%s to version %d? Pass --yes to confirm.\n", ns, name, toVersion)
				return fmt.Errorf("aborted: confirmation required")
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			cfg, err := client.NewConfigmapClient(api).RollbackConfigmap(ns, name, toVersion)
			if err != nil {
				return err
			}
			fmt.Printf("Configmap %s/%s rolled back to version %d (new HEAD = v%d)\n", ns, name, toVersion, cfg.Version)
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().IntVar(&toVersion, "to", 0, "Target historical version to roll back to")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm operation")
	return cmd
}

// renderConfigmapVersions prints a configmap's version history. Configmaps are
// plaintext, so data is available — but the version list shows metadata only
// (key count); use `rune configmap get` for the current data.
func renderConfigmapVersions(versions []*types.Configmap, format string) error {
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(versions)
	case "yaml":
		return writeYAML(versions)
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "VERSION\tKEYS\tCREATED\tUPDATED")
		for _, c := range versions {
			fmt.Fprintf(w, "%d\t%d\t%s\t%s\n", c.Version, len(c.Data), formatTime(c.CreatedAt), formatTime(c.UpdatedAt))
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

// --- delete ---

func newConfigmapDeleteCmd() *cobra.Command {
	opts := &deleteOptions{}
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a configmap",
		Long: `Alias for 'rune delete config <name>' — both share the same handler.
Deletes are recorded in the audit log.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeleteConfigmap(cmd.Context(), args[0], opts)
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace")
	cmd.Flags().BoolVar(&opts.ignoreNotFound, "ignore-not-found", false, "Don't error if the configmap doesn't exist")
	return cmd
}
