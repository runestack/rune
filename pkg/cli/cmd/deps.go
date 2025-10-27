package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// newDepsCmd creates the top-level deps command group
func newDepsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deps",
		Short: "Manage and inspect service dependencies",
	}

	cmd.AddCommand(newDepsValidateCmd())
	cmd.AddCommand(newDepsGraphCmd())
	cmd.AddCommand(newDepsCheckCmd())
	cmd.AddCommand(newDepsDependentsCmd())
	return cmd
}

func newDepsValidateCmd() *cobra.Command {
	var namespace string
	c := &cobra.Command{
		Use:   "validate <service>",
		Short: "Validate dependencies for a service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			sc := client.NewServiceClient(api)

			// Fetch the target service
			svc, err := sc.GetService(namespace, name)
			if err != nil {
				return fmt.Errorf("failed to get service %s/%s: %w", namespace, name, err)
			}

			// Validate each dependency exists
			var errs []string
			for _, d := range svc.Dependencies {
				depNS := d.Namespace
				if depNS == "" {
					depNS = namespace
				}
				if _, err := sc.GetService(depNS, d.Service); err != nil {
					errs = append(errs, fmt.Sprintf("missing dependency: %s/%s", depNS, d.Service))
				}
			}

			// Cycle detection within reachable subgraph
			cycles := detectCycles(sc, svc)
			errs = append(errs, cycles...)

			if len(errs) > 0 {
				fmt.Println("Validation errors:")
				for _, e := range errs {
					fmt.Printf("  - %s\n", e)
				}
				return errors.New("dependency validation failed")
			}

			fmt.Printf("Dependencies for %s/%s are valid\n", namespace, name)
			return nil
		},
	}
	c.Flags().StringVarP(&namespace, "namespace", "n", "default", "Namespace")
	return c
}

func newDepsGraphCmd() *cobra.Command {
	var namespace string
	var format string
	c := &cobra.Command{
		Use:   "graph <service>",
		Short: "Show dependency graph",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			sc := client.NewServiceClient(api)
			root, err := sc.GetService(namespace, name)
			if err != nil {
				return err
			}

			// BFS through dependencies
			nodes, edges := buildDepGraph(sc, root)

			if len(edges) == 0 {
				fmt.Printf("No dependencies for %s/%s\n", namespace, name)
				return nil
			}

			switch strings.ToLower(format) {
			case "dot":
				fmt.Println("digraph deps {")
				for node := range nodes {
					fmt.Printf("  \"%s\";\n", node)
				}
				for _, e := range edges {
					fmt.Printf("  \"%s\" -> \"%s\";\n", e.from, e.to)
				}
				fmt.Println("}")
			default:
				fmt.Printf("Dependency graph from %s/%s:\n", namespace, name)
				for _, e := range edges {
					fmt.Printf("- %s -> %s\n", e.from, e.to)
				}
			}
			return nil
		},
	}
	c.Flags().StringVarP(&namespace, "namespace", "n", "default", "Namespace")
	c.Flags().StringVar(&format, "format", "text", "Output format (text|dot)")
	return c
}

func newDepsCheckCmd() *cobra.Command {
	var namespace string
	c := &cobra.Command{
		Use:   "check <service>",
		Short: "Check dependency readiness",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			sc := client.NewServiceClient(api)
			hc := client.NewHealthClient(api)

			svc, err := sc.GetService(namespace, name)
			if err != nil {
				return err
			}

			notReady := []string{}
			for _, d := range svc.Dependencies {
				depNS := d.Namespace
				if depNS == "" {
					depNS = namespace
				}
				// Prefer health API; fallback to service status
				resp, err := hc.GetHealth("service", d.Service, depNS, false)
				if err == nil && len(resp.Components) == 1 {
					if resp.Components[0].Status != generated.HealthStatus_HEALTH_STATUS_HEALTHY {
						notReady = append(notReady, fmt.Sprintf("%s/%s (%s)", depNS, d.Service, resp.Components[0].Status.String()))
						continue
					}
				} else {
					depSvc, err := sc.GetService(depNS, d.Service)
					if err != nil || depSvc.Status != types.ServiceStatusRunning {
						notReady = append(notReady, fmt.Sprintf("%s/%s", depNS, d.Service))
						continue
					}
				}
			}

			if len(notReady) > 0 {
				fmt.Println("Not Ready dependencies:")
				for _, n := range notReady {
					fmt.Printf("  - %s\n", n)
				}
				return errors.New("dependencies not ready")
			}
			fmt.Printf("All dependencies for %s/%s are Ready\n", namespace, name)
			return nil
		},
	}
	c.Flags().StringVarP(&namespace, "namespace", "n", "default", "Namespace")
	return c
}

func newDepsDependentsCmd() *cobra.Command {
	var namespace string
	c := &cobra.Command{
		Use:   "dependents <service>",
		Short: "List services that depend on the target",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			sc := client.NewServiceClient(api)

			// Try all namespaces; server maps "*" to all
			services, err := sc.ListServices("*", "", "")
			if err != nil {
				// Fallback to current namespace
				services, err = sc.ListServices(namespace, "", "")
				if err != nil {
					return err
				}
			}

			targetFQ := fmt.Sprintf("%s.%s", name, namespace)
			var results []string
			for _, s := range services {
				for _, d := range s.Dependencies {
					depNS := d.Namespace
					if depNS == "" {
						depNS = s.Namespace
					}
					if (d.Service == name && depNS == namespace) || fmt.Sprintf("%s.%s", d.Service, depNS) == targetFQ {
						results = append(results, fmt.Sprintf("%s/%s", s.Namespace, s.Name))
						break
					}
				}
			}

			if len(results) == 0 {
				fmt.Printf("No dependents for %s/%s\n", namespace, name)
				return nil
			}

			fmt.Printf("Dependents of %s/%s:\n", namespace, name)
			for _, r := range results {
				fmt.Printf("  - %s\n", r)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&namespace, "namespace", "n", "default", "Namespace")
	return c
}

type edge struct{ from, to string }

func buildDepGraph(sc *client.ServiceClient, root *types.Service) (map[string]struct{}, []edge) {
	nodes := map[string]struct{}{}
	edges := []edge{}
	type qitem struct{ ns, name string }
	q := []qitem{{ns: root.Namespace, name: root.Name}}
	seen := map[string]bool{}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		key := cur.ns + "/" + cur.name
		if seen[key] {
			continue
		}
		seen[key] = true
		nodes[key] = struct{}{}
		svc, err := sc.GetService(cur.ns, cur.name)
		if err != nil {
			continue
		}
		for _, d := range svc.Dependencies {
			depNS := d.Namespace
			if depNS == "" {
				depNS = cur.ns
			}
			child := depNS + "/" + d.Service
			edges = append(edges, edge{from: key, to: child})
			q = append(q, qitem{ns: depNS, name: d.Service})
		}
	}
	return nodes, edges
}

func detectCycles(sc *client.ServiceClient, start *types.Service) []string {
	var errs []string
	type node struct{ ns, name string }
	visiting := map[string]bool{}
	visited := map[string]bool{}

	var dfs func(n node)
	dfs = func(n node) {
		key := n.ns + "/" + n.name
		if visiting[key] {
			errs = append(errs, fmt.Sprintf("cycle detected at %s", key))
			return
		}
		if visited[key] {
			return
		}
		visiting[key] = true

		svc, err := sc.GetService(n.ns, n.name)
		if err == nil {
			for _, d := range svc.Dependencies {
				depNS := d.Namespace
				if depNS == "" {
					depNS = n.ns
				}
				dfs(node{ns: depNS, name: d.Service})
			}
		}

		visiting[key] = false
		visited[key] = true
	}

	dfs(node{ns: start.Namespace, name: start.Name})
	return errs
}
