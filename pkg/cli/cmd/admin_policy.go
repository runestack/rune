package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
)

func newAdminPolicyCreateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{Use: "create", Short: "Create or update a policy",
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return fmt.Errorf("--file is required")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			// Expect a minimal YAML with name/description/rules; for MVP, parse via generated structs if using JSON, otherwise require JSON
			// To keep simple, use JSON input for now
			var p generated.Policy
			if err := json.Unmarshal(data, &p); err != nil {
				return err
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			_, err = ac.PolicyCreate(context.Background(), &generated.PolicyCreateRequest{Policy: &p})
			return err
		}}
	cmd.Flags().StringVar(&file, "file", "", "Path to policy JSON/YAML file")
	return cmd
}

func newAdminPolicyListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List policies", RunE: func(cmd *cobra.Command, args []string) error {
		api, err := newAPIClient("", "")
		if err != nil {
			return err
		}
		defer api.Close()
		ac := generated.NewAdminServiceClient(api.Conn())
		resp, err := ac.PolicyList(context.Background(), &generated.PolicyListRequest{})
		if err != nil {
			return err
		}
		for _, p := range resp.Policies {
			fmt.Println(p.Name)
		}
		return nil
	}}
}

func newAdminPolicyGetCmd() *cobra.Command {
	return &cobra.Command{Use: "get <name>", Short: "Get a policy", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		api, err := newAPIClient("", "")
		if err != nil {
			return err
		}
		defer api.Close()
		ac := generated.NewAdminServiceClient(api.Conn())
		_, err = ac.PolicyGet(context.Background(), &generated.PolicyGetRequest{Name: name})
		return err
	}}
}

func newAdminPolicyDeleteCmd() *cobra.Command {
	return &cobra.Command{Use: "delete <name>", Short: "Delete a policy", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		api, err := newAPIClient("", "")
		if err != nil {
			return err
		}
		defer api.Close()
		ac := generated.NewAdminServiceClient(api.Conn())
		_, err = ac.PolicyDelete(context.Background(), &generated.PolicyDeleteRequest{Name: name})
		return err
	}}
}

func newAdminPolicyAttachCmd() *cobra.Command {
	var subject, subjectType, policy string
	cmd := &cobra.Command{Use: "attach", Short: "Attach a policy to a subject",
		RunE: func(cmd *cobra.Command, args []string) error {
			if subject == "" || policy == "" {
				return fmt.Errorf("--subject and --policy are required")
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			_, err = ac.PolicyAttachToSubject(context.Background(), &generated.PolicyAttachToSubjectRequest{SubjectName: subject, SubjectKind: subjectType, PolicyName: policy})
			return err
		}}
	cmd.Flags().StringVar(&subject, "subject", "", "Subject name")
	cmd.Flags().StringVar(&subjectType, "subject-type", "user", "Subject type (user|service)")
	cmd.Flags().StringVar(&policy, "policy", "", "Policy name or id")
	return cmd
}

func newAdminPolicyDetachCmd() *cobra.Command {
	var subject, subjectType, policy string
	cmd := &cobra.Command{Use: "detach", Short: "Detach a policy from a subject",
		RunE: func(cmd *cobra.Command, args []string) error {
			if subject == "" || policy == "" {
				return fmt.Errorf("--subject and --policy are required")
			}
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			_, err = ac.PolicyDetachFromSubject(context.Background(), &generated.PolicyDetachFromSubjectRequest{SubjectName: subject, SubjectKind: subjectType, PolicyName: policy})
			return err
		}}
	cmd.Flags().StringVar(&subject, "subject", "", "Subject name")
	cmd.Flags().StringVar(&subjectType, "subject-type", "user", "Subject type (user|service)")
	cmd.Flags().StringVar(&policy, "policy", "", "Policy name or id")
	return cmd
}
