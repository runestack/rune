package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout [context-name]",
		Short: "Clear stored credentials for a context",
		Long: `Clear the stored access and refresh tokens for a context.

By default this clears the current context. The context entry (server, default
namespace) is kept so you can log back in; only the credentials are removed.

This is a local operation: it does not revoke the token server-side. To revoke a
refresh grant for everyone holding it, an admin runs:
  rune admin token revoke --id <token-id>   (see 'rune admin token list')`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadContextConfig()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			name := cfg.CurrentContext
			if len(args) > 0 {
				name = args[0]
			}
			ctx, ok := cfg.Contexts[name]
			if !ok {
				return fmt.Errorf("context %q not found", name)
			}
			if ctx.Token == "" && ctx.RefreshToken == "" {
				fmt.Printf("Context %q already has no stored credentials\n", name)
				return nil
			}
			ctx.Token = ""
			ctx.RefreshToken = ""
			cfg.Contexts[name] = ctx
			if err := saveContextConfig(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}
			fmt.Printf("Cleared credentials for context %q\n", name)
			return nil
		},
	}
	return cmd
}
