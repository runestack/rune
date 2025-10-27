package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	internalConfig "github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
)

func newLoginCmd() *cobra.Command {
	var server string
	var namespace string
	var token string
	var tokenFile string
	var contextName string
	var noVerify bool

	cmd := &cobra.Command{
		Use:   "login [context-name]",
		Short: "Login and create/update a context (shortcut to 'rune config set-context')",
		Long: `Login and create/update a context.

This is a shortcut for 'rune config set-context' that creates or updates a context
and optionally sets it as the current context.

If context-name is not provided, it will use "default".
If --set-current is provided, the new context will become the current context.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" && tokenFile == "" {
				return fmt.Errorf("must provide --token or --token-file")
			}

			// Set default context name
			if len(args) > 0 {
				contextName = args[0]
			} else {
				contextName = "default"
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

			// Verify credentials with server before saving context (unless skipped)
			if !noVerify {
				api, err := newAPIClient(server, token)
				if err != nil {
					return fmt.Errorf("failed to connect to server %s: %w", server, err)
				}
				defer api.Close()

				ac := generated.NewAuthServiceClient(api.Conn())
				whoCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := ac.WhoAmI(whoCtx, &generated.WhoAmIRequest{}); err != nil {
					return fmt.Errorf("failed to verify credentials with server %s: %w", server, err)
				}
			}

			// Create or update the context
			ctx := Context{
				Server:           server,
				Token:            token,
				DefaultNamespace: namespace,
			}

			// If server not specified, try to get from current context, else default to gRPC host:port
			if server == "" {
				if currentCtx, exists := config.Contexts[config.CurrentContext]; exists {
					ctx.Server = currentCtx.Server
				} else {
					ctx.Server = fmt.Sprintf("localhost:%d", internalConfig.DefaultGRPCPort)
				}
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
	cmd.Flags().StringVar(&server, "server", fmt.Sprintf("localhost:%d", internalConfig.DefaultGRPCPort), "Rune gRPC server address (host:port)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Optional default namespace")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token value")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "Path to file containing the bearer token")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "Skip server verification and just set the context")
	return cmd
}
