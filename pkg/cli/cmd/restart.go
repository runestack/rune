package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

var (
	restartNamespace  string
	restartDetach     bool
	restartTimeout    time.Duration
	restartClientAddr string
)

// restartCmd represents the restart command (in-place service restart).
var restartCmd = &cobra.Command{
	Use:   "restart <service-name>",
	Short: "Restart a service by replacing every instance in place",
	Long: `Restart a service by replacing every instance with a fresh one at the
current spec. The desired scale never dips through zero — the server stamps a
new template generation and the reconciler swaps the instances. Restarting a
stopped service starts it at its last non-zero scale.`,
	Args: cobra.ExactArgs(1),
	RunE: runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)

	restartCmd.Flags().StringVarP(&restartNamespace, "namespace", "n", "default", "Namespace of the service")
	restartCmd.Flags().BoolVarP(&restartDetach, "detach", "d", false, "Don't wait for the restart to complete (fire-and-forget)")
	restartCmd.Flags().DurationVar(&restartTimeout, "timeout", 10*time.Minute, "How long to wait for all instances to be replaced and Running")
	restartCmd.Flags().StringVar(&restartClientAddr, "api-server", "", "Address of the API server")
}

func runRestart(cmd *cobra.Command, args []string) error {
	serviceName := args[0]

	apiClient, err := newAPIClient(restartClientAddr, "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	svcClient := client.NewServiceClient(apiClient)

	// One atomic server-side operation: the service's template generation is
	// stamped and the reconciler replaces every instance (issue #140). No
	// client-side drain/scale choreography — nothing to race, nothing to
	// strand at scale 0.
	templateGen, scale, err := svcClient.RestartService(restartNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to restart service: %w", err)
	}

	fmt.Printf("↻ Restarting %s in %s (replacing %d instance(s) in place)\n",
		format.Highlight("%s", serviceName),
		format.Highlight("%s", restartNamespace),
		scale)

	if restartDetach {
		fmt.Printf("  %s detached (use `rune get instances -n %s` to watch the replacement)\n",
			format.Dim("→"), restartNamespace)
		return nil
	}

	wallStart := time.Now()
	if err := waitForRestartComplete(apiClient, serviceName, restartNamespace, templateGen, scale, restartTimeout); err != nil {
		return err
	}

	fmt.Printf("%s %s restarted (%s)\n",
		format.Success("✓"),
		format.Highlight("%s", serviceName),
		formatDuration(time.Since(wallStart)))
	return nil
}

// waitForRestartComplete polls the instance list until the restart has
// converged: exactly `scale` live instances, every one of them created at (or
// after) the stamped template generation, and every one Running. Old-template
// instances draining away show up as either stale-generation or Terminating,
// so this criterion cannot report success before the replacement actually
// happened.
func waitForRestartComplete(apiClient *client.Client, serviceName, namespace string, templateGen int64, scale int, timeout time.Duration) error {
	instanceClient := client.NewInstanceClient(apiClient)

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	lastLine := ""
	for {
		if time.Now().After(deadline) {
			problems := restartProblems(instanceClient, serviceName, namespace, templateGen)
			hint := fmt.Sprintf("run `rune get instances -n %s` or `rune describe service %s -n %s` to investigate", namespace, serviceName, namespace)
			if problems != "" {
				return fmt.Errorf("timed out after %s waiting for restart to complete; problem instances: %s — %s", timeout, problems, hint)
			}
			return fmt.Errorf("timed out after %s waiting for restart to complete — %s", timeout, hint)
		}

		instances, err := instanceClient.ListInstances(namespace, serviceName, "", "")
		if err != nil {
			// Transient list failures shouldn't abort the wait.
			<-ticker.C
			continue
		}

		var freshRunning, freshOther, stale int
		var failed []string
		for _, inst := range instances {
			if inst.Status == types.InstanceStatusDeleted {
				continue
			}
			gen := int64(0)
			if inst.Metadata != nil {
				gen = inst.Metadata.ServiceGeneration
			}
			if gen >= templateGen {
				if inst.Status == types.InstanceStatusRunning {
					freshRunning++
				} else {
					freshOther++
					if inst.Status == types.InstanceStatusFailed || inst.Status == types.InstanceStatusStalled {
						failed = append(failed, fmt.Sprintf("%s (%s)", inst.Name, inst.Status))
					}
				}
			} else {
				stale++
			}
		}

		if line := fmt.Sprintf("[rune] restarting: %d/%d replaced and ready", freshRunning, scale); line != lastLine {
			fmt.Println(line)
			lastLine = line
		}
		if len(failed) > 0 {
			fmt.Printf("  %s replacement instance(s) unhealthy: %s\n", format.Dim("⚠"), strings.Join(failed, ", "))
		}

		if freshRunning == scale && freshOther == 0 && stale == 0 {
			return nil
		}

		<-ticker.C
	}
}

// restartProblems summarizes non-Running replacement instances for the
// timeout error message.
func restartProblems(instanceClient *client.InstanceClient, serviceName, namespace string, templateGen int64) string {
	instances, err := instanceClient.ListInstances(namespace, serviceName, "", "")
	if err != nil {
		return ""
	}
	var problems []string
	for _, inst := range instances {
		if inst.Status == types.InstanceStatusDeleted || inst.Status == types.InstanceStatusRunning {
			continue
		}
		gen := int64(0)
		if inst.Metadata != nil {
			gen = inst.Metadata.ServiceGeneration
		}
		if gen >= templateGen {
			problems = append(problems, fmt.Sprintf("%s (%s)", inst.Name, inst.Status))
		}
	}
	return strings.Join(problems, ", ")
}
