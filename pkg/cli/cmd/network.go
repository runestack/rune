package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/networking/policy"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// newPolicyCmd assembles the `rune policy` command group used by
// the networking layer (RUNE-064 onwards) for ServiceNetworkPolicy
// inspection. This is distinct from `rune admin policy` (RBAC).
func newPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Inspect ServiceNetworkPolicy on a service",
	}
	cmd.AddCommand(newPolicyExplainCmd())
	cmd.AddCommand(newPolicyValidateCmd())
	return cmd
}

func newPolicyExplainCmd() *cobra.Command {
	opts := &cmdOptions{}
	var output string
	cmd := &cobra.Command{
		Use:   "explain <service>",
		Short: "Show the compiled NetworkPolicy for a service",
		Long: `Fetches a service from the API, compiles its ServiceNetworkPolicy
locally and prints the rule set the agent's proxy will enforce. This
is the operator's primary tool for answering "why was this connection
denied?".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, err := createAPIClient(opts)
			if err != nil {
				return fmt.Errorf("failed to create API client: %w", err)
			}
			defer apiClient.Close()
			svc, err := client.NewServiceClient(apiClient).GetService(opts.namespace, args[0])
			if err != nil {
				return fmt.Errorf("get service %s: %w", args[0], err)
			}
			compiled := policy.Compile(svc)
			out := compiled.Explain()
			switch output {
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			case "yaml":
				return yaml.NewEncoder(os.Stdout).Encode(out)
			default:
				return printExplain(svc, out)
			}
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "Namespace of the service")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "Output format: table | json | yaml")
	return cmd
}

func newPolicyValidateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a ServiceNetworkPolicy without applying it",
		Long: `Reads a Service spec (YAML or JSON) from --file or stdin, validates
its ServiceNetworkPolicy block (CIDR parsing, port format, peer
shape) and prints the compiled rule summary. Exit code 0 = valid.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw []byte
			var err error
			if file == "" || file == "-" {
				raw, err = readAllStdin()
			} else {
				raw, err = os.ReadFile(file)
			}
			if err != nil {
				return err
			}
			var svc types.Service
			if jerr := json.Unmarshal(raw, &svc); jerr != nil {
				if yerr := yaml.Unmarshal(raw, &svc); yerr != nil {
					return fmt.Errorf("decode service spec: %v / %v", jerr, yerr)
				}
			}
			if svc.NetworkPolicy == nil {
				fmt.Println("no network policy on service")
				return nil
			}
			if err := policy.Validate(svc.NetworkPolicy); err != nil {
				return err
			}
			compiled := policy.Compile(&svc)
			return printExplain(&svc, compiled.Explain())
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to service spec (YAML/JSON), '-' for stdin")
	return cmd
}

func printExplain(svc *types.Service, out policy.ExplainOutput) error {
	if out.Open {
		fmt.Printf("service %s/%s: no policy (open)\n", svc.Namespace, svc.ID)
		return nil
	}
	fmt.Printf("service: %s/%s\npolicy:  %s\n", out.Namespace, out.ServiceID, out.PolicyName)
	fmt.Printf("default-deny ingress=%v egress=%v\n", out.DefaultDenyIngress, out.DefaultDenyEgress)
	if len(out.Ingress) > 0 {
		fmt.Println("ingress rules:")
		for i, r := range out.Ingress {
			fmt.Printf("  [%d] peers=%v ports=%v\n", i, r.Peers, r.Ports)
		}
	}
	if len(out.Egress) > 0 {
		fmt.Println("egress rules:")
		for i, r := range out.Egress {
			fmt.Printf("  [%d] peers=%v ports=%v\n", i, r.Peers, r.Ports)
		}
	}
	return nil
}

func readAllStdin() ([]byte, error) {
	st, err := os.Stdin.Stat()
	if err != nil {
		return nil, err
	}
	if (st.Mode() & os.ModeCharDevice) != 0 {
		return nil, fmt.Errorf("no input on stdin (use --file)")
	}
	return readAll(os.Stdin)
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
