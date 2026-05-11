package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ContextConfig represents the structure of the Rune config file
type ContextConfig struct {
	CurrentContext string             `yaml:"current-context"`
	Contexts       map[string]Context `yaml:"contexts"`
}

// Context represents a single context configuration
type Context struct {
	Server           string `yaml:"server"`
	Token            string `yaml:"token"`
	DefaultNamespace string `yaml:"defaultNamespace,omitempty"`
}

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage Rune CLI contexts (server, token, default namespace)",
		Long: `Manage Rune CLI contexts.

A context bundles the server address, bearer token, and default
namespace used by every Rune CLI command. Use this group to:

- View the current context
- Set or update a context
- Switch between contexts
- List or delete contexts`,
	}

	cmd.AddCommand(newContextViewCmd())
	cmd.AddCommand(newContextSetCmd())
	cmd.AddCommand(newContextUseCmd())
	cmd.AddCommand(newContextListCmd())
	cmd.AddCommand(newContextDeleteCmd())

	return cmd
}

func newContextViewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "view",
		Short: "View current configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := loadContextConfig()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			fmt.Printf("Current Context: %s\n", config.CurrentContext)
			fmt.Println()

			if currentCtx, exists := config.Contexts[config.CurrentContext]; exists {
				fmt.Printf("Server: %s\n", currentCtx.Server)
				fmt.Printf("Token: %s...\n", maskToken(currentCtx.Token))
				if currentCtx.DefaultNamespace != "" {
					fmt.Printf("Default Namespace: %s\n", currentCtx.DefaultNamespace)
				}
			} else {
				fmt.Println("Current context not found in configuration")
			}

			return nil
		},
	}

	return cmd
}

func newContextSetCmd() *cobra.Command {
	var server string
	var token string
	var tokenFile string
	var namespace string

	cmd := &cobra.Command{
		Use:   "set [context-name]",
		Short: "Set or update a context configuration",
		Long: `Set or update a context configuration.

If context-name is not provided, it will use "default".
You must provide --server and --token (or --token-file) to configure the context.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			contextName := "default"
			if len(args) > 0 {
				contextName = args[0]
			}

			if server == "" {
				return fmt.Errorf("--server is required")
			}
			if token == "" && tokenFile == "" {
				return fmt.Errorf("must provide --token or --token-file")
			}

			// Read token from file if specified
			if token == "" && tokenFile != "" {
				b, err := os.ReadFile(tokenFile)
				if err != nil {
					return fmt.Errorf("failed to read token file: %w", err)
				}
				token = strings.TrimSpace(string(b))
			}

			// Load existing config or create new one
			config, err := loadContextConfig()
			if err != nil {
				config = &ContextConfig{
					CurrentContext: contextName,
					Contexts:       make(map[string]Context),
				}
			}

			// Normalize server address for gRPC default port
			normalizedServer := ensureDefaultGRPCPort(server)

			// Create or update the context
			ctx := Context{
				Server:           normalizedServer,
				Token:            token,
				DefaultNamespace: namespace,
			}

			config.Contexts[contextName] = ctx

			// Automatically switch to the new context
			config.CurrentContext = contextName

			// Save the configuration
			if err := saveContextConfig(config); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Context '%s' configured successfully and set as current context\n", contextName)

			return nil
		},
	}

	cmd.Flags().StringVar(&server, "server", "", "Rune API server URL (required)")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token value")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "Path to file containing the bearer token")
	cmd.Flags().StringVar(&namespace, "default-namespace", "", "Optional default namespace")

	return cmd
}

func newContextUseCmd() *cobra.Command {
	var defaultNamespace string
	cmd := &cobra.Command{
		Use:   "use [context-name]",
		Short: "Switch to a different context",
		Long: `Switch to a different context.

This command changes the current context to the specified one.
The context must already exist in your configuration.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			contextName := args[0]

			config, err := loadContextConfig()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			if _, exists := config.Contexts[contextName]; !exists {
				return fmt.Errorf("context '%s' does not exist. Use 'rune context set %s' to create it first", contextName, contextName)
			}

			// Optionally update default namespace on the selected context
			if strings.TrimSpace(defaultNamespace) != "" {
				ctx := config.Contexts[contextName]
				ctx.DefaultNamespace = defaultNamespace
				config.Contexts[contextName] = ctx
			}

			config.CurrentContext = contextName

			if err := saveContextConfig(config); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Switched to context '%s'\n", contextName)
			return nil
		},
	}

	cmd.Flags().StringVar(&defaultNamespace, "default-namespace", "", "Set default namespace for this context")
	return cmd
}

func newContextListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all available contexts",
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := loadContextConfig()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			if len(config.Contexts) == 0 {
				fmt.Println("No contexts configured")
				return nil
			}

			fmt.Printf("Available contexts:\n")
			for name, ctx := range config.Contexts {
				marker := " "
				if name == config.CurrentContext {
					marker = "*"
				}
				fmt.Printf("%s %s\n", marker, name)
				fmt.Printf("    Server: %s\n", ctx.Server)
				fmt.Printf("    Token: %s...\n", maskToken(ctx.Token))
				if ctx.DefaultNamespace != "" {
					fmt.Printf("    Default Namespace: %s\n", ctx.DefaultNamespace)
				}
				fmt.Println()
			}

			return nil
		},
	}

	return cmd
}

func newContextDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete [context-name]",
		Short: "Delete a context",
		Long: `Delete a context from your configuration.

You cannot delete the current context. Switch to a different context first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			contextName := args[0]

			config, err := loadContextConfig()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			if _, exists := config.Contexts[contextName]; !exists {
				return fmt.Errorf("context '%s' does not exist", contextName)
			}

			if contextName == config.CurrentContext {
				return fmt.Errorf("cannot delete current context '%s'. Switch to a different context first", contextName)
			}

			delete(config.Contexts, contextName)

			if err := saveContextConfig(config); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Context '%s' deleted successfully\n", contextName)
			return nil
		},
	}

	return cmd
}

// loadContextConfig loads the context configuration from file
func loadContextConfig() (*ContextConfig, error) {
	configPath := getConfigPath()

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &ContextConfig{
				CurrentContext: "default",
				Contexts:       make(map[string]Context),
			}, nil
		}
		return nil, err
	}

	var config ContextConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return &config, nil
}

// saveContextConfig saves the context configuration to file
func saveContextConfig(config *ContextConfig) error {
	configPath := getConfigPath()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// getConfigPath returns the path to the config file
func getConfigPath() string {
	if cfgFile != "" {
		return cfgFile
	}
	if envPath, ok := os.LookupEnv("RUNE_CLI_CONFIG"); ok && envPath != "" {
		return envPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./.rune/config.yaml"
	}

	return filepath.Join(home, ".rune", "config.yaml")
}
