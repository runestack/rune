package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// newSecretCmd builds the `rune secret` command group, the canonical CLI
// surface for managing secrets server-side (RUNE-103).
//
// The historical `rune get secrets` / `rune create secret` / `rune delete
// secret` commands continue to work and share the same gRPC plumbing — this
// group adds the operations that have no corresponding generic verb (reveal,
// versions, rollback) and offers a one-stop subcommand tree for operators.
func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage secrets (get, list, reveal, update, versions, rollback)",
	}
	cmd.AddCommand(newSecretGetCmd())
	cmd.AddCommand(newSecretListCmd())
	cmd.AddCommand(newSecretRevealCmd())
	cmd.AddCommand(newSecretUpdateCmd())
	cmd.AddCommand(newSecretSetCmd())
	cmd.AddCommand(newSecretUnsetCmd())
	cmd.AddCommand(newSecretDeleteCmd())
	cmd.AddCommand(newSecretVersionsCmd())
	cmd.AddCommand(newSecretRollbackCmd())
	return cmd
}

// secretFormatFlags wires the standard --output flag onto a subcommand.
func addSecretOutputFlag(cmd *cobra.Command, out *string) {
	cmd.Flags().StringVarP(out, "output", "o", "table", "Output format: table|json|yaml")
}

func addSecretNamespaceFlag(cmd *cobra.Command, ns *string) {
	cmd.Flags().StringVarP(ns, "namespace", "n", "default", "Namespace")
}

// --- get (alias for `rune get secret <name>`) ---
//
// Both shapes intentionally coexist: kubectl-shaped users reach for
// `rune get secret`, while users browsing `rune secret --help` find the full
// secret lifecycle (including reveal/versions/rollback) in one tree. To avoid
// drift, this command delegates to handleSecretGet — the same handler the
// generic `rune get` path uses — so they share rendering, label/field
// selectors, and `-o` output formats verbatim.

func newSecretGetCmd() *cobra.Command {
	opts := &getOptions{outputFormat: "table"}
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a secret's metadata (no plaintext)",
		Long: `Get returns metadata for a secret — namespace, name, version, timestamps,
and the list of data keys. To retrieve the plaintext payload use 'rune secret
reveal' (requires the secrets:reveal RBAC verb).

Alias for 'rune get secret <name>' — both share the same handler and output
formatting. Use whichever fits your muscle memory.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleSecretGet(cmd, opts, args[0])
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", "table", "Output format: table|json|yaml")
	return cmd
}

// --- list (alias for `rune get secrets`) ---

func newSecretListCmd() *cobra.Command {
	opts := &getOptions{outputFormat: "table"}
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List secrets in a namespace",
		Long: `Alias for 'rune get secrets' — both share the same handler, label/field
selectors, and output formatting.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// resourceName == "" tells handleSecretGet to list rather than get-one.
			return handleSecretGet(cmd, opts, "")
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace")
	cmd.Flags().BoolVarP(&opts.allNamespaces, "all-namespaces", "A", false, "List secrets across all namespaces")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", "table", "Output format: table|json|yaml|wide|name")
	cmd.Flags().StringVarP(&opts.labelSelector, "selector", "l", "", "Label selector (key=value,key=value)")
	cmd.Flags().StringVar(&opts.fieldSelector, "field-selector", "", "Field selector (key=value)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "Maximum number of secrets to return (0 = unlimited)")
	return cmd
}

// --- reveal ---

func newSecretRevealCmd() *cobra.Command {
	var ns, format string
	var version int
	cmd := &cobra.Command{
		Use:   "reveal <name>",
		Short: "Reveal the plaintext payload of a secret (audited)",
		Long: `Reveal returns the plaintext data map for a secret. Each call is recorded
in the server-side audit log with action=reveal (or reveal-version) and the
calling subject's identity.

With --version N, fetches the plaintext payload of a specific historical
version instead of the current head.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			sc := client.NewSecretClient(api)
			var sec *types.Secret
			if version > 0 {
				sec, err = sc.RevealSecretVersion(ns, args[0], version)
			} else {
				sec, err = sc.RevealSecret(ns, args[0])
			}
			if err != nil {
				return err
			}
			return renderSecret(sec, format, true)
		},
	}
	addSecretNamespaceFlag(cmd, &ns)
	cmd.Flags().IntVar(&version, "version", 0, "Reveal a specific historical version (default: current HEAD)")
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table|json|yaml|env")
	return cmd
}

// --- update ---

func newSecretUpdateCmd() *cobra.Command {
	var ns string
	var dataPairs []string
	var fromFile []string
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update an existing secret's data (creates a new version)",
		Long: `Update rewrites the secret's data map and bumps its version. The previous
version remains in history and can be inspected with 'rune secret versions'
or restored with 'rune secret rollback'.`,
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
			sec := &types.Secret{Name: name, Namespace: ns, Type: "static", Data: data}
			if err := client.NewSecretClient(api).UpdateSecret(sec, false); err != nil {
				return err
			}
			fmt.Printf("Secret %s/%s updated with %d data entries\n", ns, name, len(data))
			return nil
		},
	}
	addSecretNamespaceFlag(cmd, &ns)
	cmd.Flags().StringArrayVar(&dataPairs, "data", nil, "Data entry key=value (can repeat; value is taken verbatim — no comma/newline splitting)")
	cmd.Flags().StringArrayVar(&fromFile, "from-file", nil, "Read data from file: --from-file=key=path (file's bytes become the value for key — use for binary or multi-line content like PEM). Can repeat.")
	return cmd
}

// --- set (server-side merge) ---
//
// `set` is the safe way to rotate a single key in a multi-key secret. Unlike
// `update`, it preserves all other keys: the server reads the current data
// map, applies the requested upserts, and writes a new version atomically
// under a per-secret lock. The client never sees the existing keys' plaintext,
// so this requires only secrets:update (not secrets:reveal).

func newSecretSetCmd() *cobra.Command {
	var ns string
	var fromFile []string
	cmd := &cobra.Command{
		Use:   "set <name> [KEY=VALUE ...]",
		Short: "Upsert one or more keys in a secret (server-side merge)",
		Long: `Set upserts the given keys into an existing secret's data map without
touching the other keys. Each invocation creates a new version.

Use this when you want to rotate a single key — e.g. update INFRA_JWT_SECRET
without wiping INFRA_ENCRYPTION_PASSPHRASE that lives in the same secret.

Auth: requires secrets:update (NOT secrets:reveal). The server performs the
merge internally and the response is metadata-only.

Examples:
  rune secret set gateway-secrets -n prod INFRA_JWT_SECRET=new-value
  rune secret set gateway-secrets -n prod KEY1=v1 KEY2=v2
  rune secret set gateway-secrets -n prod --from-file=TLS_CERT=./cert.pem`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			pairs := args[1:]

			set := map[string]string{}
			if err := applyFromFileFlags(fromFile, set); err != nil {
				return err
			}
			for _, pair := range pairs {
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
			out, err := client.NewSecretClient(api).PatchSecret(ns, name, set, nil)
			if err != nil {
				return err
			}
			keys := keysOf(set)
			sort.Strings(keys)
			fmt.Printf("Secret %s/%s patched (v%d): set %s\n", ns, name, out.Version, strings.Join(keys, ","))
			return nil
		},
	}
	addSecretNamespaceFlag(cmd, &ns)
	cmd.Flags().StringArrayVar(&fromFile, "from-file", nil, "Read a key from a file: --from-file=key=path (file bytes become the value, useful for PEM/binary). Can repeat.")
	return cmd
}

// --- unset (server-side merge, idempotent) ---

func newSecretUnsetCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:   "unset <name> KEY [KEY ...]",
		Short: "Remove one or more keys from a secret (server-side merge)",
		Long: `Unset removes the listed keys from an existing secret's data map without
touching the other keys. Missing keys are silently ignored — re-running the
same unset is safe. Each invocation that actually removes a key creates a
new version.

Auth: requires secrets:update (NOT secrets:reveal).

Examples:
  rune secret unset gateway-secrets -n prod LEGACY_TOKEN
  rune secret unset gateway-secrets -n prod KEY1 KEY2`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			unset := args[1:]

			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			out, err := client.NewSecretClient(api).PatchSecret(ns, name, nil, unset)
			if err != nil {
				return err
			}
			sort.Strings(unset)
			fmt.Printf("Secret %s/%s patched (v%d): unset %s\n", ns, name, out.Version, strings.Join(unset, ","))
			return nil
		},
	}
	addSecretNamespaceFlag(cmd, &ns)
	return cmd
}

// keysOf returns the keys of a string map in unspecified order.
func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- delete ---

func newSecretDeleteCmd() *cobra.Command {
	opts := &deleteOptions{}
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a secret",
		Long: `Alias for 'rune delete secret <name>' — both share the same handler. Secret
deletion is unconditional (no interactive confirmation today); deletes are
recorded in the audit log.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeleteSecret(cmd.Context(), args[0], opts)
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace")
	return cmd
}

// --- versions ---

func newSecretVersionsCmd() *cobra.Command {
	var ns, format string
	cmd := &cobra.Command{
		Use:   "versions <name>",
		Short: "List historical versions of a secret (metadata only)",
		Long: `Versions returns metadata for every historical version of a secret, newest
first. Plaintext is never included; use 'rune secret reveal --version N' to
fetch the payload of a specific version.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			versions, err := client.NewSecretClient(api).ListSecretVersions(ns, args[0])
			if err != nil {
				return err
			}
			return renderSecretVersions(versions, format)
		},
	}
	addSecretNamespaceFlag(cmd, &ns)
	addSecretOutputFlag(cmd, &format)
	return cmd
}

// --- rollback ---

func newSecretRollbackCmd() *cobra.Command {
	var ns string
	var toVersion int
	var yes bool
	cmd := &cobra.Command{
		Use:   "rollback <name>",
		Short: "Rewrite a secret's HEAD to the contents of a prior version",
		Long: `Rollback fetches the named historical version and writes its data as a new
HEAD version (head+1). Old versions are retained — rollback never deletes
history. Each rollback is recorded as an audit event with metadata
fromVersion=<prev>, toVersion=<target>, newVersion=<new head>.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if toVersion <= 0 {
				return fmt.Errorf("--to is required and must be > 0")
			}
			if !yes {
				fmt.Fprintf(os.Stderr, "Rollback secret %s/%s to version %d? Pass --yes to confirm.\n", ns, name, toVersion)
				return fmt.Errorf("aborted: confirmation required")
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			sec, err := client.NewSecretClient(api).RollbackSecret(ns, name, toVersion)
			if err != nil {
				return err
			}
			fmt.Printf("Secret %s/%s rolled back to version %d (new HEAD = v%d)\n", ns, name, toVersion, sec.Version)
			return nil
		},
	}
	addSecretNamespaceFlag(cmd, &ns)
	cmd.Flags().IntVar(&toVersion, "to", 0, "Target historical version to roll back to")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm destructive operation")
	return cmd
}

// --- rendering helpers ---

// renderSecret prints a single secret. When reveal is true the Data map is
// included verbatim; otherwise only DataKeys is shown.
func renderSecret(sec *types.Secret, format string, reveal bool) error {
	if sec == nil {
		return fmt.Errorf("nil secret")
	}
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(secretView(sec, reveal))
	case "yaml":
		return writeYAML(secretView(sec, reveal))
	case "env":
		if !reveal {
			return fmt.Errorf("env output is only valid with reveal")
		}
		keys := sortedKeys(sec.Data)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, sec.Data[k])
		}
		return nil
	case "", "table":
		return renderSecretTable(sec, reveal)
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func renderSecretVersions(versions []*types.Secret, format string) error {
	switch strings.ToLower(format) {
	case "json":
		views := make([]map[string]interface{}, 0, len(versions))
		for _, s := range versions {
			views = append(views, secretView(s, false))
		}
		return writeJSON(views)
	case "yaml":
		views := make([]map[string]interface{}, 0, len(versions))
		for _, s := range versions {
			views = append(views, secretView(s, false))
		}
		return writeYAML(views)
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "VERSION\tKEYS\tCREATED\tUPDATED")
		for _, s := range versions {
			fmt.Fprintf(w, "%d\t%d\t%s\t%s\n", s.Version, len(s.DataKeys), formatTime(s.CreatedAt), formatTime(s.UpdatedAt))
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func renderSecretTable(sec *types.Secret, reveal bool) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Namespace:\t%s\n", sec.Namespace)
	fmt.Fprintf(w, "Name:\t%s\n", sec.Name)
	fmt.Fprintf(w, "Type:\t%s\n", sec.Type)
	fmt.Fprintf(w, "Version:\t%d\n", sec.Version)
	fmt.Fprintf(w, "Created:\t%s\n", formatTime(sec.CreatedAt))
	fmt.Fprintf(w, "Updated:\t%s\n", formatTime(sec.UpdatedAt))
	if reveal {
		fmt.Fprintln(w, "Data:")
		for _, k := range sortedKeys(sec.Data) {
			fmt.Fprintf(w, "  %s:\t%s\n", k, sec.Data[k])
		}
	} else {
		keys := sec.DataKeys
		sort.Strings(keys)
		fmt.Fprintf(w, "Keys:\t%s\n", strings.Join(keys, ", "))
	}
	return w.Flush()
}

// secretView projects a Secret into a stable, render-friendly map. Plaintext
// data is included only when reveal=true; otherwise only the key list is
// shown so the structure mirrors the on-the-wire metadata-only response.
func secretView(sec *types.Secret, reveal bool) map[string]interface{} {
	view := map[string]interface{}{
		"namespace": sec.Namespace,
		"name":      sec.Name,
		"type":      sec.Type,
		"version":   sec.Version,
		"createdAt": sec.CreatedAt,
		"updatedAt": sec.UpdatedAt,
		"dataKeys":  sec.DataKeys,
	}
	if reveal {
		view["data"] = sec.Data
	}
	return view
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func writeJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeYAML(v interface{}) error {
	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(v)
}
