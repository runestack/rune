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
	egressState := fmt.Sprintf("%v", out.DefaultDenyEgress)
	if out.EgressUnenforced != "" {
		egressState += " (NOT ENFORCED)"
	}
	fmt.Printf("default-deny ingress=%v egress=%s\n", out.DefaultDenyIngress, egressState)
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
		// State the boundary where the operator is actually looking.
		// Egress is enforced in the service proxy, so it constrains
		// service-to-service traffic only; it is not an exfiltration
		// boundary until the kernel path lands (#194). Printed only
		// when egress rules exist, so it reaches exactly the people
		// who need it.
		if out.EgressUnenforced != "" {
			fmt.Printf("WARNING: these egress rules are NOT enforced — %s\n", out.EgressUnenforced)
		} else {
			fmt.Println("note: egress is enforced for service-to-service traffic.")
			fmt.Println("      Traffic to the internet or to raw container IPs is not filtered.")
		}
	}
	return nil
}

// egressUnenforcedReason reports why a service's egress rules will not
// be applied, or "" when they will be.
//
// Egress evaluation resolves a connection's source IP to a service
// identity, and only the Docker runner reports instance IPs — so a
// process-runtime service never enters the identity table and its
// egress rules are silently ignored. Printing "default-deny egress"
// for such a service is a claim Rune does not honour. See
// https://github.com/runestack/rune/issues/197.
func egressUnenforcedReason(svc *types.Service, out policy.ExplainOutput) string {
	if len(out.Egress) == 0 || svc == nil {
		return ""
	}
	if svc.Runtime == types.RuntimeTypeProcess {
		return "runtime: process — the agent cannot identify process instances, so no egress rule is applied (issue #197)"
	}
	return ""
}
