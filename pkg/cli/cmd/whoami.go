package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// whoamiOutput represents the structured output for whoami command
type whoamiOutput struct {
	CurrentContext   string   `json:"currentContext" yaml:"currentContext"`
	Server           string   `json:"server" yaml:"server"`
	DefaultNamespace string   `json:"defaultNamespace" yaml:"defaultNamespace"`
	Token            string   `json:"token" yaml:"token"`
	Status           string   `json:"status" yaml:"status"`
	SubjectID        string   `json:"subjectId,omitempty" yaml:"subjectId,omitempty"`
	Name             string   `json:"name,omitempty" yaml:"name,omitempty"`
	Email            string   `json:"email,omitempty" yaml:"email,omitempty"`
	Policies         []string `json:"policies,omitempty" yaml:"policies,omitempty"`
	Note             string   `json:"note,omitempty" yaml:"note,omitempty"`
}

func newWhoAmICmd() *cobra.Command {
	var outputFormat string

	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show current identity and context",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get current context information by loading config directly
			config, err := loadContextConfig()
			currentContext := "default"
			if err == nil && config != nil {
				currentContext = config.CurrentContext
			}

			server := viper.GetString("contexts.default.server")
			token := viper.GetString("contexts.default.token")
			ns := viper.GetString("contexts.default.defaultNamespace")
			if ns == "" {
				ns = "default"
			}

			// Prepare output structure
			output := &whoamiOutput{
				CurrentContext:   currentContext,
				Server:           server,
				DefaultNamespace: ns,
				Token:            maskToken(token),
			}

			api, err := newAPIClient("", "")
			if err != nil {
				// Fallback to local display only
				output.Status = "Not connected to server"
				output.Note = "Run 'rune login' to authenticate"

				if outputFormat == "json" || outputFormat == "yaml" {
					return outputStructured(output, outputFormat)
				}

				// Default table output
				fmt.Printf("Current Context: %s\n", output.CurrentContext)
				fmt.Printf("Server: %s\n", output.Server)
				fmt.Printf("Default Namespace: %s\n", output.DefaultNamespace)
				fmt.Printf("Token: %s\n", output.Token)
				fmt.Printf("Status: %s\n", output.Status)
				fmt.Println()
				fmt.Printf("Note: %s\n", output.Note)
				return nil
			}
			defer api.Close()

			ac := generated.NewAuthServiceClient(api.Conn())
			resp, err := ac.WhoAmI(context.Background(), &generated.WhoAmIRequest{})
			if err != nil || resp == nil || resp.SubjectId == "" {
				// Fallback to local
				output.Status = "Not authenticated"
				output.Note = "Run 'rune login' to authenticate"

				if outputFormat == "json" || outputFormat == "yaml" {
					return outputStructured(output, outputFormat)
				}

				// Default table output
				fmt.Printf("Current Context: %s\n", output.CurrentContext)
				fmt.Printf("Server: %s\n", output.Server)
				fmt.Printf("Default Namespace: %s\n", output.DefaultNamespace)
				fmt.Printf("Token: %s\n", output.Token)
				fmt.Printf("Status: %s\n", output.Status)
				fmt.Println()
				fmt.Printf("Note: %s\n", output.Note)
				return nil
			}

			// Populate authenticated user information
			output.Status = "Authenticated"
			output.SubjectID = resp.SubjectId

			if resp.SubjectName != "" {
				output.Name = resp.SubjectName
			}

			if resp.SubjectEmail != "" {
				output.Email = resp.SubjectEmail
			}

			if len(resp.Policies) > 0 {
				output.Policies = resp.Policies
			}

			// Output based on format
			if outputFormat == "json" || outputFormat == "yaml" {
				return outputStructured(output, outputFormat)
			} else if outputFormat != "table" {
				return fmt.Errorf("unsupported output format: %s (supported: table, json, yaml)", outputFormat)
			}

			// Default table output
			fmt.Printf("Current Context: %s\n", output.CurrentContext)
			fmt.Printf("Server: %s\n", output.Server)
			fmt.Printf("Default Namespace: %s\n", output.DefaultNamespace)
			fmt.Printf("Token: %s\n", output.Token)
			fmt.Printf("Status: %s\n", output.Status)
			fmt.Printf("Subject ID: %s\n", output.SubjectID)

			if output.Name != "" {
				fmt.Printf("Name: %s\n", output.Name)
			}

			if output.Email != "" {
				fmt.Printf("Email: %s\n", output.Email)
			}

			if len(output.Policies) > 0 {
				fmt.Printf("Policies: %v\n", output.Policies)
			}

			return nil
		},
	}

	// Add output format flag
	cmd.Flags().StringVarP(&outputFormat, "output", "o", "table", "Output format (table, json, yaml)")

	return cmd
}

// outputStructured outputs the whoami information in the specified format
func outputStructured(output *whoamiOutput, format string) error {
	switch format {
	case "json":
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(data))
	case "yaml":
		data, err := yaml.Marshal(output)
		if err != nil {
			return fmt.Errorf("failed to marshal YAML: %w", err)
		}
		fmt.Println(string(data))
	default:
		return fmt.Errorf("unsupported output format: %s", format)
	}
	return nil
}

func maskToken(t string) string {
	if len(t) <= 6 {
		return "******"
	}
	return t[:3] + "******" + t[len(t)-3:]
}
