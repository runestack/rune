package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
)

func newAdminTokenCreateCmd() *cobra.Command {
	var desc, outFile string
	var ttl time.Duration
	var subjectID, subjectType, name string
	var policies []string
	var refresh bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new token and write it to a file",
		RunE: func(cmd *cobra.Command, args []string) error {
			if outFile == "" {
				return fmt.Errorf("--out-file is required")
			}
			// Build client
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			ac := generated.NewAuthServiceClient(api.Conn())
			kind := ""
			if refresh {
				kind = "refresh"
			}
			req := &generated.CreateTokenRequest{
				Name:        name,
				SubjectId:   subjectID,
				SubjectType: subjectType,
				Policies:    policies,
				TtlSeconds:  int64(ttl / time.Second),
				Description: desc,
				Kind:        kind,
			}
			ctx, cancel := api.Context()
			defer cancel()
			resp, err := ac.CreateToken(ctx, req)
			if err != nil {
				return err
			}
			if resp.Secret == "" {
				return fmt.Errorf("server did not return a token secret")
			}
			if err := os.WriteFile(outFile, []byte(resp.Secret), 0o600); err != nil {
				return fmt.Errorf("failed to write token to %s: %w", outFile, err)
			}
			fmt.Printf("Token created: id=%s name=%s written to %s\n", resp.Id, resp.Name, outFile)
			return nil
		},
	}
	cmd.Flags().StringVar(&desc, "description", "", "Description")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "Time-to-live (e.g., 1h). 0 means no expiry")
	cmd.Flags().StringVar(&outFile, "out-file", "", "Output file for token (0600)")
	cmd.Flags().StringVar(&subjectID, "subject-id", "", "Subject ID (user/service account id)")
	cmd.Flags().StringVar(&subjectType, "subject-type", "user", "Subject type (user|service)")
	cmd.Flags().StringVar(&name, "name", "", "Token name")
	cmd.Flags().StringSliceVar(&policies, "policy", nil, "Default policy to attach on auto-create (repeatable)")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Issue a refresh grant (RUNE-201) instead of a long-lived token; exchange it at /v1/auth/refresh for short-lived access tokens")
	return cmd
}

func newAdminTokenRevokeCmd() *cobra.Command {
	var tokenIdArg string
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a token",
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			ac := generated.NewAuthServiceClient(api.Conn())
			ctx, cancel := api.Context()
			defer cancel()
			resp, err := ac.RevokeToken(ctx, &generated.RevokeTokenRequest{Id: tokenIdArg})
			if err != nil {
				return err
			}
			if !resp.Revoked {
				return fmt.Errorf("token not revoked")
			}
			fmt.Println("Token revoked")
			return nil
		},
	}
	cmd.Flags().StringVar(&tokenIdArg, "id", "", "Token ID")
	return cmd
}

func newAdminTokenListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tokens (admin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			ctx, cancel := api.Context()
			defer cancel()
			resp, err := ac.TokenList(ctx, &generated.TokenListRequest{})
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.Tokens)
			}
			for _, t := range resp.Tokens {
				var exp string
				if t.ExpiresAt == 0 {
					exp = "-"
				} else {
					exp = time.Unix(t.ExpiresAt, 0).Format(time.RFC3339)
				}
				issued := time.Unix(t.IssuedAt, 0).Format(time.RFC3339)
				kind := t.Kind
				if kind == "" {
					kind = "static"
				}
				lastUsed := "-"
				if t.LastUsedAt != 0 {
					lastUsed = time.Unix(t.LastUsedAt, 0).Format(time.RFC3339)
				}
				fmt.Printf("%s\t%s\t%s\t%s\t%s\tlast-used=%s\trevoked=%v\n", t.Name, kind, t.SubjectId, issued, exp, lastUsed, t.Revoked)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output JSON")
	return cmd
}
