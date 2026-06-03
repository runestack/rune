package cmd

import (
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
)

func newAdminUserCreateCmd() *cobra.Command {
	var name, email string
	var policies []string
	cmd := &cobra.Command{Use: "create", Short: "Create or update a user",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			ctx, cancel := api.Context()
			defer cancel()
			_, err = ac.UserCreate(ctx, &generated.UserCreateRequest{Name: name, Email: email, Policies: policies})
			return err
		}}
	cmd.Flags().StringVar(&name, "name", "", "User name")
	cmd.Flags().StringVar(&email, "email", "", "User email")
	cmd.Flags().StringSliceVar(&policies, "policy", nil, "Policy to attach (repeatable)")
	return cmd
}

func newAdminUserEnrollCmd() *cobra.Command {
	var policies []string
	var subjectType string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "enroll <name>",
		Short: "Issue a one-time enrollment code so a user can self-provision a session",
		Long: `Issue a one-time, short-lived enrollment code (RUNE-201).

The code is NOT a usable credential — it only authorizes creating one refresh
grant for the named subject with the given policies. The user redeems it on
their own machine with 'rune login --enroll <code>', receiving the refresh
secret directly, so you never handle a working credential.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAuthServiceClient(api.Conn())
			ctx, cancel := api.Context()
			defer cancel()
			resp, err := ac.Enroll(ctx, &generated.EnrollRequest{
				SubjectName: args[0],
				SubjectType: subjectType,
				Policies:    policies,
				TtlSeconds:  int64(ttl / time.Second),
			})
			if err != nil {
				return err
			}
			fmt.Printf("Enrollment code: %s\n", resp.Code)
			if resp.ExpiresAt != 0 {
				fmt.Printf("Expires:         %s\n", time.Unix(resp.ExpiresAt, 0).Format(time.RFC3339))
			}
			fmt.Printf("\nShare this code with %q. They run:\n  rune login --enroll %s\n", args[0], resp.Code)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&policies, "policy", nil, "Policy to attach to the subject (repeatable)")
	cmd.Flags().StringVar(&subjectType, "type", "user", "Subject type (user|service)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "Code lifetime (default 10m)")
	return cmd
}

func newAdminUserListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List users", RunE: func(cmd *cobra.Command, args []string) error {
		api, err := newAPIClient("", "")
		if err != nil {
			return err
		}
		defer api.Close()
		ac := generated.NewAdminServiceClient(api.Conn())
		ctx, cancel := api.Context()
		defer cancel()
		resp, err := ac.UserList(ctx, &generated.UserListRequest{})
		if err != nil {
			return err
		}
		for _, u := range resp.Users {
			fmt.Println(u.Name)
		}
		return nil
	}}
}
