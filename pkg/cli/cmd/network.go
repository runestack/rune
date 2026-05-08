// Package cmd: helpers for rendering compiled ServiceNetworkPolicy.
//
// Surfaced via the verb-first `rune get netpolicy <service>` command;
// the `policy.Compile(...).Explain()` output is the operator's primary
// tool for answering "why was this connection denied?".
package cmd

import (
	"fmt"

	"github.com/runestack/rune/pkg/networking/policy"
	"github.com/runestack/rune/pkg/types"
)

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
