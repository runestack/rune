// Package cmd — `rune storageclass` (alias `sc`) noun-tree subcommands.
//
// Mirrors the `rune secret` precedent at pkg/cli/cmd/secret.go: a single
// command group whose subcommands wrap the typed StorageClassClient. The
// underlying gRPC service is cluster-scoped, so none of these subcommands
// take a --namespace flag.
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
	"gopkg.in/yaml.v3"
)

// newStorageClassCmd builds the `rune storageclass` command group.
func newStorageClassCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "storageclass",
		Aliases: []string{"sc", "storageclasses"},
		Short:   "Manage storage classes (list, get, create, delete, set-default)",
	}
	cmd.AddCommand(newStorageClassListCmd())
	cmd.AddCommand(newStorageClassGetCmd())
	cmd.AddCommand(newStorageClassCreateCmd())
	cmd.AddCommand(newStorageClassDeleteCmd())
	cmd.AddCommand(newStorageClassSetDefaultCmd())
	return cmd
}

// --- list ---

func newStorageClassListCmd() *cobra.Command {
	var format string
	var labelSelector string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List storage classes",
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			selector := parseLabelSelectorString(labelSelector)
			classes, err := client.NewStorageClassClient(api).ListStorageClasses(selector)
			if err != nil {
				return err
			}
			sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })
			return renderStorageClasses(classes, format)
		},
	}
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table|json|yaml|name")
	cmd.Flags().StringVarP(&labelSelector, "selector", "l", "", "Label selector (key=value,key=value)")
	return cmd
}

// --- get ---

func newStorageClassGetCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a storage class by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			sc, err := client.NewStorageClassClient(api).GetStorageClass(args[0])
			if err != nil {
				return err
			}
			return renderStorageClass(sc, format)
		},
	}
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table|json|yaml")
	return cmd
}

// --- create ---

func newStorageClassCreateCmd() *cobra.Command {
	var fromFile string
	cmd := &cobra.Command{
		Use:   "create -f <file>",
		Short: "Create a storage class from a YAML or JSON spec file",
		Long: `Reads a StorageClass spec from --file (YAML or JSON) and creates it on the
server. The file should contain a single StorageClass; the cluster-default
flag (default: true) is honoured by the API server which will atomically
demote any prior Default class.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromFile == "" {
				return fmt.Errorf("--file is required")
			}
			sc, err := readStorageClassFile(fromFile)
			if err != nil {
				return err
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			if err := client.NewStorageClassClient(api).CreateStorageClass(sc); err != nil {
				return err
			}
			fmt.Printf("StorageClass %s created\n", sc.Name)
			return nil
		},
	}
	cmd.Flags().StringVarP(&fromFile, "file", "f", "", "Path to YAML or JSON spec file")
	return cmd
}

// --- delete ---

func newStorageClassDeleteCmd() *cobra.Command {
	var cascade bool
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"remove", "rm"},
		Short:   "Delete a storage class by name",
		Long: `Deletes a cluster-scoped StorageClass. By default the API server
refuses to delete a class that is still referenced by one or more
Volumes; pass --cascade to bypass this safety check.

--cascade does NOT delete the dependent volumes; it only removes the
StorageClass row. Existing volumes will keep a now-dangling
StorageClassName until the operator addresses them.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			if err := client.NewStorageClassClient(api).DeleteStorageClass(args[0], cascade); err != nil {
				return err
			}
			fmt.Printf("StorageClass %s deleted\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&cascade, "cascade", false, "Delete even if volumes still reference this storage class")
	return cmd
}

// --- set-default ---

func newStorageClassSetDefaultCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-default <name>",
		Short: "Mark a storage class as the cluster default",
		Long: `Promotes the named storage class to Default: true. The API server (via
the volume controller's invariant enforcer) atomically clears the Default
flag on any other class, so the cluster always has at most one default.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			scc := client.NewStorageClassClient(api)
			sc, err := scc.GetStorageClass(args[0])
			if err != nil {
				return err
			}
			if sc.Default {
				fmt.Printf("StorageClass %s is already the cluster default\n", sc.Name)
				return nil
			}
			sc.Default = true
			if err := scc.UpdateStorageClass(sc); err != nil {
				return err
			}
			fmt.Printf("StorageClass %s set as cluster default\n", sc.Name)
			return nil
		},
	}
	return cmd
}

// --- file loading ---

// readStorageClassFile loads a single StorageClass spec from a YAML or JSON
// file. JSON is a subset of YAML, so a single yaml.Unmarshal handles both.
func readStorageClassFile(path string) (*types.StorageClass, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var sc types.StorageClass
	if err := yaml.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if sc.Name == "" {
		return nil, fmt.Errorf("%s: storage class name is required", path)
	}
	if sc.Driver == "" {
		return nil, fmt.Errorf("%s: storage class driver is required", path)
	}
	return &sc, nil
}

// --- rendering helpers ---

func renderStorageClasses(classes []*types.StorageClass, format string) error {
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(classes)
	case "yaml":
		return writeYAML(classes)
	case "name":
		for _, c := range classes {
			fmt.Println(c.Name)
		}
		return nil
	case "", "table":
		if len(classes) == 0 {
			fmt.Println("No storage classes found")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tDRIVER\tDEFAULT\tRECLAIM\tAGE")
		for _, c := range classes {
			defaultStr := "-"
			if c.Default {
				defaultStr = "yes"
			}
			reclaim := string(c.ReclaimPolicy)
			if reclaim == "" {
				reclaim = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				c.Name, c.Driver, defaultStr, reclaim, formatAgeTable(c.CreatedAt))
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func renderStorageClass(sc *types.StorageClass, format string) error {
	if sc == nil {
		return fmt.Errorf("nil storage class")
	}
	switch strings.ToLower(format) {
	case "json":
		return writeJSON(sc)
	case "yaml":
		return writeYAML(sc)
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "Name:\t%s\n", sc.Name)
		fmt.Fprintf(w, "Driver:\t%s\n", sc.Driver)
		fmt.Fprintf(w, "Default:\t%t\n", sc.Default)
		if sc.ReclaimPolicy != "" {
			fmt.Fprintf(w, "ReclaimPolicy:\t%s\n", sc.ReclaimPolicy)
		}
		fmt.Fprintf(w, "Created:\t%s\n", formatTime(sc.CreatedAt))
		fmt.Fprintf(w, "Updated:\t%s\n", formatTime(sc.UpdatedAt))
		if len(sc.Parameters) > 0 {
			fmt.Fprintln(w, "Parameters:")
			for _, k := range sortedKeys(sc.Parameters) {
				fmt.Fprintf(w, "  %s:\t%s\n", k, sc.Parameters[k])
			}
		}
		if len(sc.Labels) > 0 {
			fmt.Fprintln(w, "Labels:")
			for _, k := range sortedKeys(sc.Labels) {
				fmt.Fprintf(w, "  %s:\t%s\n", k, sc.Labels[k])
			}
		}
		if len(sc.AllowedTopologies) > 0 {
			fmt.Fprintf(w, "AllowedTopologies:\t%d selector(s)\n", len(sc.AllowedTopologies))
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

// parseLabelSelectorString parses a "k=v,k=v" string into a map. Unparseable
// entries are silently dropped (mirrors how the existing get command treats
// the --selector flag downstream).
func parseLabelSelectorString(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		if k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
