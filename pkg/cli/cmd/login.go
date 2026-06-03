package cmd

import (
	"bufio"
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
	var defaultNamespace string
	var legacyNamespace string
	var token string
	var tokenFile string
	var tokenStdin bool
	var contextName string
	var noVerify bool
	var asRefresh bool

	cmd := &cobra.Command{
		Use:   "login [context-name]",
		Short: "Login and create/update a context (shortcut to 'rune context set')",
		Long: `Login and create/update a context.

This is a shortcut for 'rune context set' that creates or updates a context
and optionally sets it as the current context.

If context-name is not provided, it will use "default".
If --set-current is provided, the new context will become the current context.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" && tokenFile == "" && !tokenStdin {
				return fmt.Errorf("must provide --token, --token-file, or --token-stdin")
			}

			// Resolve --default-namespace, accepting deprecated --namespace as a fallback.
			// Cobra's MarkDeprecated already prints a deprecation notice when --namespace is set.
			if cmd.Flags().Changed("namespace") && !cmd.Flags().Changed("default-namespace") {
				defaultNamespace = legacyNamespace
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

			// Read token from stdin if requested. Designed for CI: callers
			// pipe the secret in (`echo "$TOKEN" | rune login --token-stdin`)
			// so it never appears on the process argv where it could be
			// captured by /proc, ps, shell history, or CI log scrubbers.
			if token == "" && tokenStdin {
				r := bufio.NewReader(cmd.InOrStdin())
				line, err := r.ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("failed to read token from stdin: %w", err)
				}
				token = strings.TrimSpace(line)
				if token == "" {
					return fmt.Errorf("empty token read from stdin")
				}
			}

			// Load existing config or create new one
			config, err := loadContextConfig()
			if err != nil {
				config = &ContextConfig{
					CurrentContext: contextName,
					Contexts:       make(map[string]Context),
				}
			}

			// RUNE-201 refresh-grant login: exchange the grant for an initial
			// access token (this also verifies it), and store the rotated grant
			// so the client can transparently renew the session later.
			var refreshTokenVal string
			if asRefresh {
				api, err := newAPIClient(server, "")
				if err != nil {
					return fmt.Errorf("failed to connect to server %s: %w", server, err)
				}
				defer api.Close()

				ac := generated.NewAuthServiceClient(api.Conn())
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				resp, err := ac.Refresh(rctx, &generated.RefreshRequest{RefreshToken: token})
				if err != nil {
					return fmt.Errorf("failed to exchange refresh grant with server %s: %w", server, err)
				}
				refreshTokenVal = resp.GetRefreshToken()
				token = resp.GetAccessToken() // store the minted access token as the bearer
			} else if !noVerify {
				// Verify credentials with server before saving context.
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
				RefreshToken:     refreshTokenVal,
				DefaultNamespace: defaultNamespace,
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
	cmd.Flags().StringVar(&defaultNamespace, "default-namespace", "", "Default namespace stored in this context (used by future commands)")
	cmd.Flags().StringVar(&legacyNamespace, "namespace", "", "Deprecated: alias for --default-namespace")
	_ = cmd.Flags().MarkDeprecated("namespace", "use --default-namespace instead")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token value")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "Path to file containing the bearer token")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "Read the bearer token from stdin (recommended for CI)")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "Skip server verification and just set the context")
	cmd.Flags().BoolVar(&asRefresh, "refresh", false, "Treat the provided token as a RUNE-201 refresh grant: exchange it for an access token and store it for transparent session renewal")
	return cmd
}
