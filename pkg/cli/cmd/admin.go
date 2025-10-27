package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
)

func newAdminCmd() *cobra.Command {
	admin := &cobra.Command{Use: "admin", Short: "Administrative operations (bootstrap, token, user, policy)"}
	admin.AddCommand(newAdminBootstrapCmd())
	admin.AddCommand(newAdminTokenCmd())
	admin.AddCommand(newAdminUserCmd())
	admin.AddCommand(newAdminPolicyCmd())
	admin.AddCommand(newAdminRegistriesCmd())
	return admin
}

func newAdminBootstrapCmd() *cobra.Command {
	var outFile string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap root management token (server-enforced local-only if configured)",
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			resp, err := ac.AdminBootstrap(context.Background(), &generated.AdminBootstrapRequest{})
			if err != nil {
				return err
			}
			if resp.TokenSecret == "" {
				return fmt.Errorf("server did not return a token secret")
			}
			if outFile != "" {
				if err := os.WriteFile(outFile, []byte(resp.TokenSecret), 0o600); err != nil {
					return fmt.Errorf("failed to write token: %w", err)
				}
				fmt.Println("Bootstrap token written to", outFile)
				return nil
			}
			fmt.Println(resp.TokenSecret)
			return nil
		},
	}
	cmd.Flags().StringVar(&outFile, "out-file", "", "Write token to file (0600). If empty, print to stdout")
	return cmd
}

func newAdminTokenCmd() *cobra.Command {
	c := &cobra.Command{Use: "token", Short: "Manage tokens (admin)"}
	c.AddCommand(newAdminTokenCreateCmd())
	c.AddCommand(newAdminTokenRevokeCmd())
	c.AddCommand(newAdminTokenListCmd())
	return c
}

func newAdminUserCmd() *cobra.Command {
	c := &cobra.Command{Use: "user", Short: "Manage users (subjects)"}
	c.AddCommand(newAdminUserCreateCmd())
	c.AddCommand(newAdminUserListCmd())
	return c
}

func newAdminPolicyCmd() *cobra.Command {
	c := &cobra.Command{Use: "policy", Short: "Manage policies"}
	c.AddCommand(newAdminPolicyCreateCmd())
	c.AddCommand(newAdminPolicyListCmd())
	c.AddCommand(newAdminPolicyGetCmd())
	c.AddCommand(newAdminPolicyDeleteCmd())
	c.AddCommand(newAdminPolicyAttachCmd())
	c.AddCommand(newAdminPolicyDetachCmd())
	return c
}

func newAdminRegistriesCmd() *cobra.Command {
	c := &cobra.Command{Use: "registries", Short: "Manage registry authentication"}
	c.AddCommand(newAdminRegistriesListCmd())
	c.AddCommand(newAdminRegistriesShowCmd())
	c.AddCommand(newAdminRegistriesAddCmd())
	c.AddCommand(newAdminRegistriesUpdateCmd())
	c.AddCommand(newAdminRegistriesRemoveCmd())
	c.AddCommand(newAdminRegistriesBootstrapAuthCmd())
	c.AddCommand(newAdminRegistriesTestCmd())
	c.AddCommand(newAdminRegistriesStatusCmd())
	return c
}

func newAdminRegistriesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured registries",
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			resp, err := ac.ListRegistries(context.Background(), &generated.ListRegistriesRequest{})
			if err != nil {
				return err
			}
			if len(resp.GetRegistries()) == 0 {
				fmt.Println("No registries configured")
				return nil
			}
			b, _ := json.MarshalIndent(resp.Registries, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
}

func newAdminRegistriesShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show a registry configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			resp, err := ac.GetRegistry(context.Background(), &generated.GetRegistryRequest{Name: args[0]})
			if err != nil {
				return err
			}
			b, _ := json.MarshalIndent(resp.Registry, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
}

func newAdminRegistriesAddCmd() *cobra.Command {
	var name, registry, rtype, username, password, token, region, fromSecret, manage string
	var bootstrap, immutable bool
	var data map[string]string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || registry == "" {
				return fmt.Errorf("--name and --registry are required")
			}
			rc := &generated.RegistryConfig{Name: name, Registry: registry, Auth: &generated.RegistryAuthConfig{Type: rtype, Username: username, Password: password, Token: token, Region: region, FromSecret: fromSecret, Bootstrap: bootstrap, Manage: manage, Immutable: immutable, Data: data}}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			_, err = ac.AddRegistry(context.Background(), &generated.AddRegistryRequest{Registry: rc})
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Registry name")
	cmd.Flags().StringVar(&registry, "registry", "", "Registry host (or wildcard)")
	cmd.Flags().StringVar(&rtype, "type", "", "Auth type (basic|token|ecr|dockerconfigjson)")
	cmd.Flags().StringVar(&username, "username", "", "Username for basic auth")
	cmd.Flags().StringVar(&password, "password", "", "Password for basic auth")
	cmd.Flags().StringVar(&token, "token", "", "Token for token auth")
	cmd.Flags().StringVar(&region, "region", "", "Region for ECR")
	cmd.Flags().StringVar(&fromSecret, "from-secret", "", "Secret ref (ns/name or shorthand)")
	cmd.Flags().BoolVar(&bootstrap, "bootstrap-auth", false, "Bootstrap credentials into secret if missing")
	cmd.Flags().StringVar(&manage, "manage-auth", "", "Secret management (create|update|ignore)")
	cmd.Flags().BoolVar(&immutable, "immutable", false, "Do not overwrite existing secret")
	cmd.Flags().StringToStringVar(&data, "data", map[string]string{}, "Key=value pairs for bootstrap data")
	return cmd
}

func newAdminRegistriesUpdateCmd() *cobra.Command {
	var name, registry, rtype, username, password, token, region, fromSecret, manage string
	var bootstrap, immutable bool
	var data map[string]string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update a registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			rc := &generated.RegistryConfig{Name: name, Registry: registry, Auth: &generated.RegistryAuthConfig{Type: rtype, Username: username, Password: password, Token: token, Region: region, FromSecret: fromSecret, Bootstrap: bootstrap, Manage: manage, Immutable: immutable, Data: data}}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			_, err = ac.UpdateRegistry(context.Background(), &generated.UpdateRegistryRequest{Registry: rc})
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Registry name")
	cmd.Flags().StringVar(&registry, "registry", "", "Registry host (or wildcard)")
	cmd.Flags().StringVar(&rtype, "type", "", "Auth type (basic|token|ecr|dockerconfigjson)")
	cmd.Flags().StringVar(&username, "username", "", "Username for basic auth")
	cmd.Flags().StringVar(&password, "password", "", "Password for basic auth")
	cmd.Flags().StringVar(&token, "token", "", "Token for token auth")
	cmd.Flags().StringVar(&region, "region", "", "Region for ECR")
	cmd.Flags().StringVar(&fromSecret, "from-secret", "", "Secret ref (ns/name or shorthand)")
	cmd.Flags().BoolVar(&bootstrap, "bootstrap-auth", false, "Bootstrap credentials into secret if missing")
	cmd.Flags().StringVar(&manage, "manage-auth", "", "Secret management (create|update|ignore)")
	cmd.Flags().BoolVar(&immutable, "immutable", false, "Do not overwrite existing secret")
	cmd.Flags().StringToStringVar(&data, "data", map[string]string{}, "Key=value pairs for bootstrap data")
	return cmd
}

func newAdminRegistriesRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			_, err = ac.RemoveRegistry(context.Background(), &generated.RemoveRegistryRequest{Name: args[0]})
			return err
		},
	}
}

func newAdminRegistriesBootstrapAuthCmd() *cobra.Command {
	var name, rtype string
	var all bool
	cmd := &cobra.Command{
		Use:   "bootstrap-auth",
		Short: "Bootstrap credentials for registries",
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			_, err = ac.BootstrapAuth(context.Background(), &generated.BootstrapAuthRequest{Name: name, Type: rtype, All: all})
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Specific registry name")
	cmd.Flags().StringVar(&rtype, "type", "", "Registry type filter")
	cmd.Flags().BoolVar(&all, "all", false, "Apply to all registries")
	return cmd
}

func newAdminRegistriesTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Test registry connectivity/auth",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			resp, err := ac.TestRegistry(context.Background(), &generated.TestRegistryRequest{Name: args[0]})
			if err != nil {
				return err
			}
			if resp.GetOk() {
				fmt.Println("ok")
			} else {
				fmt.Println("fail:", resp.GetMessage())
			}
			return nil
		},
	}
}

func newAdminRegistriesStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show registry runtime status",
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			resp, err := ac.RegistriesStatus(context.Background(), &generated.RegistriesStatusRequest{})
			if err != nil {
				return err
			}
			if len(resp.GetRegistries()) == 0 {
				fmt.Println("No registries configured.")
				return nil
			}
			b, _ := json.MarshalIndent(resp.Registries, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
}
