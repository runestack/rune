package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// createCmd is the umbrella command for quick create
func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create resources quickly",
		Run:   func(cmd *cobra.Command, args []string) { _ = cmd.Help() },
	}
	cmd.AddCommand(newCreateSecretCmd())
	cmd.AddCommand(newCreateConfigCmd())
	cmd.AddCommand(newCreateNamespaceCmd())
	return cmd
}

func newCreateSecretCmd() *cobra.Command {
	var namespace string
	var dataPairs []string
	var fromFile []string
	var createNamespace bool
	cmd := &cobra.Command{
		Use:   "secret <name>",
		Short: "Create a secret from key=value pairs or from a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			data := map[string]string{}

			if err := applyFromFileFlags(fromFile, data); err != nil {
				return err
			}

			// Add any additional data pairs from command line
			for _, pair := range dataPairs {
				k, v, err := splitPair(pair)
				if err != nil {
					return err
				}
				data[k] = v
			}

			// Validate that we have some data
			if len(data) == 0 {
				return fmt.Errorf("no data provided. Use --data flags or --from-file")
			}

			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			sc := client.NewSecretClient(api)
			sec := &types.Secret{Name: name, Namespace: namespace, Type: "static", Data: data}
			if err := sc.CreateSecret(sec, createNamespace); err != nil {
				return err
			}
			fmt.Printf("Secret %s/%s created with %d data entries\n", namespace, name, len(data))
			return nil
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringArrayVar(&dataPairs, "data", nil, "Data entry key=value (can repeat; value is taken verbatim — no comma/newline splitting)")
	cmd.Flags().StringArrayVar(&fromFile, "from-file", nil, "Read data from file: --from-file=key=path (file's bytes become the value for key — use for binary or multi-line content like PEM). Can repeat.")
	cmd.Flags().BoolVar(&createNamespace, "create-namespace", false, "Create the namespace if it doesn't exist")
	return cmd
}

func newCreateConfigCmd() *cobra.Command {
	var namespace string
	var dataPairs []string
	var fromFile []string
	var createNamespace bool
	cmd := &cobra.Command{
		Use:   "config <name>",
		Short: "Create a config from key=value pairs or from a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			data := map[string]string{}

			if err := applyFromFileFlags(fromFile, data); err != nil {
				return err
			}

			// Add any additional data pairs from command line
			for _, pair := range dataPairs {
				k, v, err := splitPair(pair)
				if err != nil {
					return err
				}
				data[k] = v
			}

			// Validate that we have some data
			if len(data) == 0 {
				return fmt.Errorf("no data provided. Use --data flags or --from-file")
			}

			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			cc := client.NewConfigmapClient(api)
			cfg := &types.Configmap{Name: name, Namespace: namespace, Data: data}
			if err := cc.CreateConfigmap(cfg, createNamespace); err != nil {
				return err
			}
			fmt.Printf("Config %s/%s created with %d data entries\n", namespace, name, len(data))
			return nil
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Namespace")
	cmd.Flags().StringArrayVar(&dataPairs, "data", nil, "Data entry key=value (can repeat; value is taken verbatim — no comma/newline splitting)")
	cmd.Flags().StringArrayVar(&fromFile, "from-file", nil, "Read data from file: --from-file=key=path (file's bytes become the value for key — use for binary or multi-line content). Can repeat.")
	cmd.Flags().BoolVar(&createNamespace, "create-namespace", false, "Create the namespace if it doesn't exist")
	return cmd
}

func newCreateNamespaceCmd() *cobra.Command {
	var labels []string
	cmd := &cobra.Command{
		Use:   "namespace <name>",
		Short: "Create a namespace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			// Parse labels
			labelMap := make(map[string]string)
			for _, label := range labels {
				k, v, err := splitPair(label)
				if err != nil {
					return err
				}
				labelMap[k] = v
			}

			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			nc := client.NewNamespaceClient(api)
			ns := &types.Namespace{
				Name:      name,
				Labels:    labelMap,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}

			if err := nc.CreateNamespace(ns); err != nil {
				return err
			}

			fmt.Printf("Namespace %s created\n", name)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Labels key=value (can repeat)")
	return cmd
}

func splitPair(pair string) (string, string, error) {
	parts := strings.SplitN(pair, "=", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid data format: %s (expected key=value)", pair)
	}
	return parts[0], parts[1], nil
}

// applyFromFileFlags walks each --from-file value and merges its
// contents into data. Each spec MUST take the form "key=path" — the
// file's full byte contents (newlines and all) become the value for
// "key". Used for binary or multi-line content (PEM certs, TLS keys,
// raw config files).
//
// Mirrors `kubectl create secret generic --from-file=<key>=<path>`.
func applyFromFileFlags(specs []string, data map[string]string) error {
	for _, spec := range specs {
		if spec == "" {
			continue
		}
		eq := strings.IndexByte(spec, '=')
		if eq <= 0 {
			return fmt.Errorf("--from-file %q: expected key=path", spec)
		}
		key := spec[:eq]
		path := spec[eq+1:]
		if path == "" {
			return fmt.Errorf("--from-file %q: empty path after '='", spec)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("--from-file %q: %w", spec, err)
		}
		data[key] = string(content)
	}
	return nil
}

func init() { rootCmd.AddCommand(newCreateCmd()) }
