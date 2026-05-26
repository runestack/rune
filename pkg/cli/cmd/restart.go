package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/utils"
	"github.com/spf13/cobra"
)

var (
	restartNamespace         string
	restartDetach            bool
	restartTimeout           time.Duration
	restartDrainTimeout      time.Duration
	restartNoProgressTimeout time.Duration
	restartClientAddr        string
)

// restartCmd represents the restart command (service bounce).
var restartCmd = &cobra.Command{
	Use:   "restart <service-name>",
	Short: "Restart a service by scaling it to 0 and back to its previous scale",
	Args:  cobra.ExactArgs(1),
	RunE:  runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)

	restartCmd.Flags().StringVarP(&restartNamespace, "namespace", "n", "default", "Namespace of the service")
	restartCmd.Flags().BoolVarP(&restartDetach, "detach", "d", false, "Don't wait for the restart to complete (fire-and-forget)")
	restartCmd.Flags().DurationVar(&restartTimeout, "timeout", 10*time.Minute, "Whole-operation budget. Drain gets up to 30% (capped by --drain-timeout); the rest goes to the start-up wait. A stuck drain can no longer burn the full timeout.")
	restartCmd.Flags().DurationVar(&restartDrainTimeout, "drain-timeout", 0, "Explicit cap on the drain (scale-to-0) phase. Default 0 means derive from --timeout (30%).")
	restartCmd.Flags().DurationVar(&restartNoProgressTimeout, "no-progress-timeout", 0, "If >0, detach the start-up wait when running/pending/failed counts have not changed for this long AND at least one instance is Failed. Default off.")
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

	svc, err := svcClient.GetService(restartNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service %s/%s: %w", restartNamespace, serviceName, err)
	}
	current := svc.Scale

	// Stopped → just scale up to the last non-zero scale.
	if current == 0 {
		desired := svc.Metadata.LastNonZeroScale
		if desired <= 0 {
			desired = 1
		}
		fmt.Printf("↻ Restarting %s in %s (stopped → %d)\n",
			format.Highlight("%s", serviceName),
			format.Highlight("%s", restartNamespace),
			desired)
		return doScale(apiClient, svcClient, serviceName, restartNamespace, desired,
			"Starting", restartDetach, restartTimeout)
	}

	// Normal restart: drain to 0, then scale back up.
	fmt.Printf("↻ Restarting %s in %s (%d → 0 → %d)\n",
		format.Highlight("%s", serviceName),
		format.Highlight("%s", restartNamespace),
		current, current)

	if restartDetach {
		// Fire and forget. We still need to send the scale-down request and
		// queue a scale-up; the orchestrator handles that as one operation.
		// For simplicity (and since the orchestrator currently treats these as
		// independent), do scale-down now and warn that scale-up requires a
		// follow-up. Most users will just want the synchronous flow.
		downReq := &generated.ScaleServiceRequest{
			Name:      serviceName,
			Namespace: restartNamespace,
			Scale:     0,
			Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
		}
		if _, err := svcClient.ScaleServiceWithRequest(downReq); err != nil {
			return fmt.Errorf("failed to scale down service: %w", err)
		}
		upReq := &generated.ScaleServiceRequest{
			Name:      serviceName,
			Namespace: restartNamespace,
			Scale:     utils.ToInt32NonNegative(current),
			Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
		}
		if _, err := svcClient.ScaleServiceWithRequest(upReq); err != nil {
			return fmt.Errorf("failed to queue scale up: %w", err)
		}
		fmt.Printf("  %s detached (use `rune status %s -n %s` to check)\n",
			format.Dim("→"), serviceName, restartNamespace)
		return nil
	}

	wallStart := time.Now()

	// Guard against the operator-visible failure mode where the
	// drain succeeds (or the drain wait times out / is cancelled)
	// and the scale-back-up never gets sent — the service is then
	// silently stranded at scale 0 and operators have to debug
	// "why is my service gone" without a clear signal that they
	// did it themselves. The defer below restores the scale on
	// any error path before we have positively queued the
	// scale-up request.
	scaleUpQueued := false
	defer func() {
		if scaleUpQueued {
			return
		}
		fmt.Printf("  %s restart aborted before scale-up; restoring scale to %d\n",
			format.Dim("⚠"), current)
		restoreReq := &generated.ScaleServiceRequest{
			Name:      serviceName,
			Namespace: restartNamespace,
			Scale:     utils.ToInt32NonNegative(current),
			Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
		}
		if _, restoreErr := svcClient.ScaleServiceWithRequest(restoreReq); restoreErr != nil {
			fmt.Printf("  %s failed to restore scale: %v (run `rune scale %s -n %s --replicas %d` manually)\n",
				format.Dim("✗"), restoreErr, serviceName, restartNamespace, current)
		}
	}()

	// Split the --timeout budget between the two phases. The original
	// code gave each phase the full --timeout, so a stuck drain could
	// burn 2× the user-facing budget before failing. Cap drain at
	// --drain-timeout (or 30% of --timeout if unset) and give the
	// remainder to the start-up wait.
	drainBudget, startBudget := splitRestartBudget(restartTimeout, restartDrainTimeout)

	// Phase 1: drain.
	downReq := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: restartNamespace,
		Scale:     0,
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(downReq); err != nil {
		return fmt.Errorf("failed to scale down service: %w", err)
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), drainBudget)
		defer cancel()
		renderer := newPhaseRenderer("Draining", 0)
		if err := waitForScalingComplete(apiClient, ctx, serviceName, restartNamespace, 0, renderer, waitOptions{}); err != nil {
			return err
		}
	}

	// Phase 2: start back up to the previous scale.
	upReq := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: restartNamespace,
		Scale:     utils.ToInt32NonNegative(current),
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(upReq); err != nil {
		return fmt.Errorf("failed to scale up service: %w", err)
	}
	scaleUpQueued = true // server has the desired scale; deferred restore is a no-op now.
	{
		ctx, cancel := context.WithTimeout(context.Background(), startBudget)
		defer cancel()
		renderer := newPhaseRenderer("Starting", current)
		if err := waitForScalingComplete(apiClient, ctx, serviceName, restartNamespace, current, renderer, waitOptions{
			noProgressTimeout: restartNoProgressTimeout,
		}); err != nil {
			return err
		}
	}

	fmt.Printf("%s %s restarted (%s)\n",
		format.Success("✓"),
		format.Highlight("%s", serviceName),
		formatDuration(time.Since(wallStart)))
	return nil
}

// doScale is the single-phase helper used for the stopped-service code path.
// The renderer's own finish line is the summary — no extra "ready" line.
func doScale(apiClient *client.Client, svcClient *client.ServiceClient, name, namespace string, target int, label string, detach bool, timeout time.Duration) error {
	req := &generated.ScaleServiceRequest{
		Name:      name,
		Namespace: namespace,
		Scale:     utils.ToInt32NonNegative(target),
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(req); err != nil {
		return fmt.Errorf("failed to scale service: %w", err)
	}
	if detach {
		fmt.Printf("  %s detached (use `rune status %s -n %s` to check)\n",
			format.Dim("→"), name, namespace)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	renderer := newPhaseRenderer(label, target)
	return waitForScalingComplete(apiClient, ctx, name, namespace, target, renderer, waitOptions{
		noProgressTimeout: restartNoProgressTimeout,
	})
}

// splitRestartBudget divides the whole-operation timeout between the
// drain phase and the start-up wait. Behavior:
//   - drainOverride > 0: that's the drain cap; start gets total - drain
//     (clamped to a 10s minimum so the start phase always gets a chance).
//   - drainOverride == 0: drain gets 30% of total, start gets 70%.
//
// 30/70 isn't sacred — it matches the empirical observation that drains
// usually finish quickly (a few seconds for a healthy stop, ~30s if the
// runner has to SIGKILL) while start-ups can legitimately take longer
// (image pull, health probe initial delays).
func splitRestartBudget(total, drainOverride time.Duration) (drain, start time.Duration) {
	if drainOverride > 0 {
		drain = drainOverride
	} else {
		drain = total * 3 / 10
	}
	if drain >= total {
		// Pathological --drain-timeout >= --timeout. Give start a 10s
		// floor so the user still sees a problem summary if the start
		// phase has its own issues.
		drain = total - 10*time.Second
		if drain < 0 {
			drain = total / 2
		}
	}
	start = total - drain
	if start < 10*time.Second {
		start = 10 * time.Second
	}
	return drain, start
}
