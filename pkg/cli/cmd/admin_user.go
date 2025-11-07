package cmd

import (
	"context"
	"fmt"

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
			_, err = ac.UserCreate(context.Background(), &generated.UserCreateRequest{Name: name, Email: email, Policies: policies})
			return err
		}}
	cmd.Flags().StringVar(&name, "name", "", "User name")
	cmd.Flags().StringVar(&email, "email", "", "User email")
	cmd.Flags().StringSliceVar(&policies, "policy", nil, "Policy to attach (repeatable)")
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
		resp, err := ac.UserList(context.Background(), &generated.UserListRequest{})
		if err != nil {
			return err
		}
		for _, u := range resp.Users {
			fmt.Println(u.Name)
		}
		return nil
	}}
}
