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
	var fromFile string
	var createNamespace bool
	cmd := &cobra.Command{
		Use:   "secret <name>",
		Short: "Create a secret from key=value pairs or from a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			data := map[string]string{}

			// Read data from file if specified
			if fromFile != "" {
				fileData, err := readDataFromFile(fromFile)
				if err != nil {
					return fmt.Errorf("failed to read file %s: %w", fromFile, err)
				}
				data = fileData
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
	cmd.Flags().StringSliceVar(&dataPairs, "data", nil, "Data entries key=value (can repeat)")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "Read data from file (key=value format, one per line)")
	cmd.Flags().BoolVar(&createNamespace, "create-namespace", false, "Create the namespace if it doesn't exist")
	return cmd
}

func newCreateConfigCmd() *cobra.Command {
	var namespace string
	var dataPairs []string
	var fromFile string
	var createNamespace bool
	cmd := &cobra.Command{
		Use:   "config <name>",
		Short: "Create a config from key=value pairs or from a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			data := map[string]string{}

			// Read data from file if specified
			if fromFile != "" {
				fileData, err := readDataFromFile(fromFile)
				if err != nil {
					return fmt.Errorf("failed to read file %s: %w", fromFile, err)
				}
				data = fileData
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
	cmd.Flags().StringSliceVar(&dataPairs, "data", nil, "Data entries key=value (can repeat)")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "Read data from file (key=value format, one per line)")
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

// readDataFromFile reads key=value pairs from a file
func readDataFromFile(filename string) (map[string]string, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	data := make(map[string]string)
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue // Skip empty lines and comments
		}

		k, v, err := splitPair(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		data[k] = v
	}

	return data, nil
}

func init() { rootCmd.AddCommand(newCreateCmd()) }
