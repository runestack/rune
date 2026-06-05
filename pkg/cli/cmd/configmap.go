package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// newConfigmapCmd builds the `rune configmap` command group — the configmap
// counterpart to `rune secret`. Configmaps are plaintext and unversioned-by-
// reveal (there is no encryption, so no `reveal`; the server keeps no per-
// version history RPC, so no `versions`/`rollback`). The lifecycle that does
// apply — get/list/update/set/unset/delete — mirrors the secret group so the
// two read the same.
//
// The historical `rune get config` / `rune create config` / `rune delete
// config` commands continue to work and share the same gRPC plumbing.
func newConfigmapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "configmap",
		Aliases: []string{"configmaps"},
		Short:   "Manage configmaps (get, list, update, set, unset, delete)",
	}
	cmd.AddCommand(newConfigmapGetCmd())
	cmd.AddCommand(newConfigmapListCmd())
	cmd.AddCommand(newConfigmapUpdateCmd())
	cmd.AddCommand(newConfigmapSetCmd())
	cmd.AddCommand(newConfigmapUnsetCmd())
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

// --- set (read-merge-write upsert) ---
//
// Unlike `rune secret set` (a server-side atomic merge via PatchSecret), the
// configmap API has no patch RPC, so `set` reads the current configmap, applies
// the upserts client-side, and writes the merged map back with UpdateConfigmap.
// Last-writer-wins under concurrent edits — acceptable for plaintext config.

func newConfigmapSetCmd() *cobra.Command {
	var ns string
	var fromFile []string
	cmd := &cobra.Command{
		Use:   "set <name> [KEY=VALUE ...]",
		Short: "Upsert one or more keys in a configmap",
		Long: `Set upserts the given keys into an existing configmap's data map without
touching the other keys. Each invocation creates a new version.

Note: the configmap API has no server-side merge, so this reads the current
configmap and writes back the merged map (last-writer-wins). Use this to
change individual keys without re-specifying the whole map.

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
			cc := client.NewConfigmapClient(api)
			cfg, err := cc.GetConfigmap(ns, name)
			if err != nil {
				return err
			}
			if cfg.Data == nil {
				cfg.Data = map[string]string{}
			}
			for k, v := range set {
				cfg.Data[k] = v
			}
			if err := cc.UpdateConfigmap(cfg); err != nil {
				return err
			}
			keys := keysOf(set)
			sort.Strings(keys)
			fmt.Printf("Configmap %s/%s set %s%s\n", ns, name, strings.Join(keys, ","), versionSuffix(cc, ns, name))
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringArrayVar(&fromFile, "from-file", nil, "Read a key from a file: --from-file=key=path (file bytes become the value). Can repeat.")
	return cmd
}

// --- unset (read-merge-write removal, idempotent) ---

func newConfigmapUnsetCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:   "unset <name> KEY [KEY ...]",
		Short: "Remove one or more keys from a configmap",
		Long: `Unset removes the listed keys from an existing configmap's data map without
touching the other keys. Missing keys are silently ignored — re-running the
same unset is safe.

Note: the configmap API has no server-side merge, so this reads the current
configmap and writes back the reduced map (last-writer-wins).

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
			cc := client.NewConfigmapClient(api)
			cfg, err := cc.GetConfigmap(ns, name)
			if err != nil {
				return err
			}
			removed := false
			for _, k := range unset {
				if _, ok := cfg.Data[k]; ok {
					delete(cfg.Data, k)
					removed = true
				}
			}
			if !removed {
				fmt.Printf("Configmap %s/%s unchanged (no matching keys)\n", ns, name)
				return nil
			}
			if err := cc.UpdateConfigmap(cfg); err != nil {
				return err
			}
			sort.Strings(unset)
			fmt.Printf("Configmap %s/%s unset %s%s\n", ns, name, strings.Join(unset, ","), versionSuffix(cc, ns, name))
			return nil
		},
	}
	cmd.Flags().StringVarP(&ns, "namespace", "n", "default", "Namespace")
	return cmd
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

// versionSuffix best-effort fetches the configmap's new version for the
// confirmation line (e.g. " (v3)"). Returns "" if it can't be read — the
// mutation already succeeded, so a missing version is cosmetic.
func versionSuffix(cc *client.ConfigmapClient, ns, name string) string {
	if cfg, err := cc.GetConfigmap(ns, name); err == nil && cfg.Version > 0 {
		return fmt.Sprintf(" (v%d)", cfg.Version)
	}
	return ""
}
