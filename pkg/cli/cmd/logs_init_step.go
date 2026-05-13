// RUNE-121 S6 — `rune logs <svc> --init-step <name>` surface.
package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/runestack/rune/pkg/api/client"
)

// printInitStepLogs walks the named service's instances and prints
// each instance's InitStepState row for opts.initStep. We deliberately
// do not stream container logs here — the log subsystem cannot yet
// resolve InitStepState.LogRef. That lands in a follow-up; for now
// users get the structured state (status/exit/attempts/reason/message)
// which is what they need to debug a stuck Initializing service.
//
// The targetArg may be either "<svc>" or the explicit "service/<svc>"
// form accepted by the streaming path.
func printInitStepLogs(apiClient *client.Client, opts *logsOptions, targetArg string) error {
	svcName := stripResourcePrefix(targetArg, "service/")
	if svcName == "" {
		return fmt.Errorf("--init-step requires a service name")
	}

	sc := client.NewServiceClient(apiClient)
	svc, err := sc.GetService(opts.namespace, svcName)
	if err != nil {
		return fmt.Errorf("failed to get service %s/%s: %w", opts.namespace, svcName, err)
	}
	if svc == nil {
		return fmt.Errorf("service %s/%s not found", opts.namespace, svcName)
	}

	// Confirm the step is declared on the service before we walk
	// instances — a typo here is the most common user error.
	declared := false
	for _, st := range svc.InitSteps {
		if st.Name == opts.initStep {
			declared = true
			break
		}
	}
	if !declared {
		names := make([]string, 0, len(svc.InitSteps))
		for _, st := range svc.InitSteps {
			names = append(names, st.Name)
		}
		if len(names) == 0 {
			return fmt.Errorf("service %s/%s has no init steps", svc.Namespace, svc.Name)
		}
		return fmt.Errorf("service %s/%s has no init step %q (have: %s)",
			svc.Namespace, svc.Name, opts.initStep, strings.Join(names, ", "))
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "INSTANCE\tSTATUS\tEXIT\tATTEMPTS\tREASON\tMESSAGE")

	rows := 0
	for _, inst := range svc.Instances {
		for _, st := range inst.InitStates {
			if st.Name != opts.initStep {
				continue
			}
			rows++
			reason := st.Reason
			if reason == "" {
				reason = "-"
			}
			msg := st.Message
			if msg == "" {
				msg = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
				inst.Name, st.Status, st.ExitCode, st.Attempts, reason, msg)
		}
	}
	if rows == 0 {
		fmt.Fprintf(os.Stderr,
			"no init state recorded for step %q on any instance of %s/%s yet\n",
			opts.initStep, svc.Namespace, svc.Name)
	}
	fmt.Fprintln(os.Stderr,
		"\nNote: streaming init step container/process logs is not yet wired;\n"+
			"this view shows the structured state recorded by the controller.")
	return nil
}

// stripResourcePrefix returns name with the given prefix removed if
// present, else name unchanged. Used to accept both "<svc>" and
// "service/<svc>" forms.
func stripResourcePrefix(name, prefix string) string {
	if strings.HasPrefix(name, prefix) {
		return strings.TrimPrefix(name, prefix)
	}
	return name
}
