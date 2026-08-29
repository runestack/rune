package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/upgrade"
	"github.com/runestack/rune/pkg/version"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type upgradeOptions struct {
	clientOnly bool
	serverOnly bool
	yes        bool
}

// Timing: the stage RPC must outlive the server's ~4-minute CDN-504 retry
// budget (the default 30s call timeout would cancel the download before
// the retry ever runs), and the watch deadline covers stage + the
// applier's own checksum fetch + restart + verify + a possible rollback.
// Both are deliberately constants, not flags — on expiry the output says
// exactly what to check.
const (
	upgradeStageTimeout  = 6 * time.Minute
	upgradeWatchDeadline = 8 * time.Minute
)

func newUpgradeCmd() *cobra.Command {
	opts := &upgradeOptions{}
	cmd := &cobra.Command{
		Use:   "upgrade [version]",
		Short: "Upgrade Rune itself — the connected server, then this CLI",
		Long: `Upgrade Rune itself (server and CLI) to a published release.
To update a service, cast it.

With no version, upgrades to the newest release. The server is upgraded
first and verified healthy before the CLI replaces its own binary, so a
failed server upgrade never leaves a newer CLI against an older server.
Passing a version older than the running one is a downgrade and asks for
explicit confirmation; the server host additionally enforces a root-owned
version floor.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 && args[0] != "latest" {
				target = args[0]
			}
			if opts.clientOnly && opts.serverOnly {
				return fmt.Errorf("--client and --server are mutually exclusive")
			}
			return runUpgrade(cmd.Context(), opts, target)
		},
	}
	cmd.Flags().BoolVar(&opts.clientOnly, "client", false, "Upgrade only this CLI")
	cmd.Flags().BoolVar(&opts.serverOnly, "server", false, "Upgrade only the connected server")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Skip confirmation prompts")
	return cmd
}

// upgradePlan is what one invocation decided to do, shared across the
// plan/confirm/execute steps.
type upgradePlan struct {
	target         string
	checksums      upgrade.Checksums
	server         *generated.GetServerVersionResponse
	doServer       bool // a server half will run
	serverAtTarget bool
	clientAtTarget bool
	allowDowngrade bool
}

func runUpgrade(ctx context.Context, opts *upgradeOptions, target string) error {
	hc := &http.Client{Timeout: 2 * time.Minute}

	// Probe the server unless the user scoped this to the client. An
	// unreachable or unconfigured server degrades to a client-only
	// upgrade — most CLI users on a team are not the operator.
	var server *generated.GetServerVersionResponse
	var api *client.Client
	if !opts.clientOnly {
		server, api = probeServerVersion()
		if server == nil {
			if opts.serverOnly {
				return fmt.Errorf("no reachable server: check your context (rune context list) or drop --server")
			}
			fmt.Println("No reachable server context — upgrading the CLI only.")
		}
	}
	if api != nil {
		defer api.Close()
	}

	plan, err := buildUpgradePlan(ctx, hc, opts, server, target)
	if err != nil {
		return err
	}
	if plan == nil {
		fmt.Println("Already up to date.")
		return nil
	}
	if err := confirmUpgradePlan(plan, opts); err != nil {
		return err
	}
	return executeUpgradePlan(ctx, hc, api, plan, opts)
}

func buildUpgradePlan(ctx context.Context, hc *http.Client, opts *upgradeOptions, server *generated.GetServerVersionResponse, target string) (*upgradePlan, error) {
	clientVer := version.Version

	// The newest release is the newest whose assets are fully uploaded —
	// release creation and asset upload are not atomic, and GitHub's
	// /releases/latest would exclude prereleases (i.e. every current Rune
	// release) entirely.
	if target == "" {
		if _, err := upgrade.ParseVersion(clientVer); err != nil && server == nil {
			return nil, fmt.Errorf("this CLI reports version %q (a from-source build), so \"newest\" has no baseline — pass an explicit version", clientVer)
		}
		required := []string{upgrade.CLIAsset(runtime.GOOS, runtime.GOARCH)}
		if server != nil {
			required = append(required, upgrade.ServerAsset(server.GetArch()))
		}
		var err error
		target, err = upgrade.ResolveNewest(ctx, hc, required...)
		if err != nil {
			return nil, fmt.Errorf("resolving the newest release: %w", err)
		}
	}
	if _, err := upgrade.ParseVersion(target); err != nil {
		return nil, err
	}
	checksums, err := upgrade.FetchChecksums(ctx, hc, target)
	if err != nil {
		return nil, err
	}

	plan := &upgradePlan{
		target:         target,
		checksums:      checksums,
		server:         server,
		doServer:       server != nil && !opts.clientOnly,
		serverAtTarget: server != nil && server.GetVersion() == target,
		clientAtTarget: clientVer == target,
	}

	fmt.Printf("Client  %s (%s/%s)\n", clientVer, runtime.GOOS, runtime.GOARCH)
	if plan.doServer {
		fmt.Printf("Server  %s (%s/%s) — context %q\n", server.GetVersion(), server.GetOs(), server.GetArch(), currentContextName())
	}
	fmt.Printf("Target  %s\n\n", target)

	if (!plan.doServer || plan.serverAtTarget) && (plan.clientAtTarget || opts.serverOnly) {
		return nil, nil // nothing to do
	}
	return plan, nil
}

func confirmUpgradePlan(plan *upgradePlan, opts *upgradeOptions) error {
	if plan.doServer && !plan.serverAtTarget {
		if err := confirmDowngrade(plan.server.GetVersion(), plan.target, opts.yes, &plan.allowDowngrade); err != nil {
			return err
		}
		fmt.Println("Ingress and service-to-service traffic pause for ~10s while the server")
		fmt.Println("restarts (longer if the upgrade rolls back). Instances keep running.")
		fmt.Println()
	}
	if opts.yes {
		return nil
	}
	what := "client"
	switch {
	case plan.doServer && !plan.serverAtTarget && opts.serverOnly:
		what = "server"
	case plan.doServer && !plan.serverAtTarget:
		what = "server, then client"
	}
	if !confirmPrompt(fmt.Sprintf("Upgrade %s to %s?", what, plan.target)) {
		return fmt.Errorf("aborted")
	}
	return nil
}

func executeUpgradePlan(ctx context.Context, hc *http.Client, api *client.Client, plan *upgradePlan, opts *upgradeOptions) error {
	switch {
	case plan.doServer && !plan.serverAtTarget:
		proceeded, err := upgradeServer(ctx, api, plan.server, plan.checksums, plan.target, plan.allowDowngrade, opts)
		if err != nil {
			return err
		}
		if proceeded {
			postRestartSweep(api)
		}
	case plan.doServer:
		fmt.Printf("  server: already at %s\n", plan.target)
	}

	if opts.serverOnly {
		fmt.Printf("✓ upgraded to %s\n", plan.target)
		return nil
	}
	if plan.clientAtTarget {
		fmt.Printf("  client: already at %s\n", plan.target)
		fmt.Printf("✓ %s\n", plan.target)
		return nil
	}
	if err := upgradeClient(ctx, hc, plan.checksums, plan.target); err != nil {
		return err
	}
	fmt.Printf("✓ upgraded to %s\n", plan.target)
	return nil
}

// probeServerVersion best-effort probes the configured context. Returns
// (nil, nil) when there is no reachable server; the caller degrades.
func probeServerVersion() (*generated.GetServerVersionResponse, *client.Client) {
	api, err := newAPIClient("", "")
	if err != nil {
		return nil, nil
	}
	hc := generated.NewHealthServiceClient(api.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := hc.GetServerVersion(ctx, &generated.GetServerVersionRequest{})
	if err != nil {
		_ = api.Close()
		return nil, nil
	}
	return resp, api
}

// upgradeServer stages the upgrade over the RPC and watches the restart
// window. Returns whether the server half actually ran (false = degraded
// to client-only); a hard error aborts the command.
func upgradeServer(ctx context.Context, api *client.Client, server *generated.GetServerVersionResponse, checksums upgrade.Checksums, target string, allowDowngrade bool, opts *upgradeOptions) (bool, error) {
	if server.GetOs() != "linux" {
		return false, fmt.Errorf("server reports %s/%s; server self-upgrade supports linux only", server.GetOs(), server.GetArch())
	}
	digest, err := checksums.Digest(upgrade.ServerAsset(server.GetArch()))
	if err != nil {
		return false, err
	}

	fmt.Printf("  server: staging %s ...\n", target)
	ac := generated.NewAdminServiceClient(api.Conn())
	rctx, cancel := api.ContextWithTimeout(upgradeStageTimeout)
	defer cancel()
	_, err = ac.UpgradeServer(rctx, &generated.UpgradeServerRequest{
		Version:        target,
		Sha256:         digest,
		AllowDowngrade: allowDowngrade,
	})
	stagedUncertain := false
	if err != nil {
		proceed, watchAnyway, herr := classifyStageError(err, target, opts)
		if herr != nil {
			return false, herr
		}
		if !proceed {
			return false, nil // degraded to client-only; message already printed
		}
		stagedUncertain = watchAnyway
	}
	if !stagedUncertain {
		fmt.Println("  server: staged (sha256 verified)")
	}
	fmt.Println("  server: applying — connection will drop while runed restarts")

	return true, watchServerUpgrade(ctx, api, server.GetVersion(), target)
}

// classifyStageError maps an UpgradeServer failure to what to do next:
// proceed+watch (the restart may have raced the reply), degrade to a
// client-only upgrade with an explanation, or fail hard.
func classifyStageError(err error, target string, opts *upgradeOptions) (proceed, watchAnyway bool, hard error) {
	st, ok := status.FromError(err)
	if !ok {
		return true, true, nil
	}
	sshOneLiner := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/%s/scripts/upgrade-server.sh | sudo bash -s -- --version %s --refresh-unit",
		upgrade.Repo, target, target)

	switch st.Code() {
	case codes.PermissionDenied:
		msg := "server upgrades need an admin token — upgrading your CLI only"
		if opts.serverOnly {
			return false, false, fmt.Errorf("%s", strings.Replace(msg, " — upgrading your CLI only", "", 1))
		}
		fmt.Printf("  server: %s\n", msg)
		return false, false, nil
	case codes.Unimplemented:
		// A pre-RUNE-321 server: same remedy as missing units.
		return degradeWithOneLiner("this server predates in-band upgrade", sshOneLiner, opts)
	case codes.FailedPrecondition:
		switch reason := upgrade.PreconditionReason(st.Message()); reason {
		case upgrade.ReasonInProgress:
			return false, false, fmt.Errorf("an upgrade is already in progress on the server: %s", st.Message())
		case upgrade.ReasonNoSystemd:
			return degradeWithOneLiner("the server does not run under systemd (dev mode?), so it cannot self-upgrade", "", opts)
		case upgrade.ReasonUnitsMissing:
			return degradeWithOneLiner("the server is missing its upgrade units (one-time setup)", sshOneLiner, opts)
		default:
			return false, false, fmt.Errorf("server refused the upgrade: %s", st.Message())
		}
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		// The reply may have raced the restart: treat as possibly staged
		// and watch rather than reporting failure for a success.
		fmt.Println("  server: connection dropped during staging — watching for the new version anyway")
		return true, true, nil
	default:
		return false, false, fmt.Errorf("staging upgrade: %s", st.Message())
	}
}

func degradeWithOneLiner(why, oneLiner string, opts *upgradeOptions) (bool, bool, error) {
	if opts.serverOnly {
		if oneLiner != "" {
			return false, false, fmt.Errorf("%s; run this on the host once:\n\n  %s", why, oneLiner)
		}
		return false, false, fmt.Errorf("%s", why)
	}
	fmt.Printf("  server: %s — upgrading your CLI only\n", why)
	if oneLiner != "" {
		fmt.Printf("  server: to enable in-band upgrades, run this on the host once:\n\n    %s\n\n", oneLiner)
	}
	return false, false, nil
}

// watchServerUpgrade polls the public GetServerVersion through the restart
// window until the target version answers ready, then reports. On the
// deadline it distinguishes the terminal states instead of guessing.
func watchServerUpgrade(ctx context.Context, api *client.Client, fromVersion, target string) error {
	hc := generated.NewHealthServiceClient(api.Conn())
	deadline := time.Now().Add(upgradeWatchDeadline)
	var downSince time.Time
	lastVersion, lastReady := "", false

	for time.Now().Before(deadline) {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := hc.GetServerVersion(pctx, &generated.GetServerVersionRequest{})
		cancel()
		switch {
		case err != nil:
			if downSince.IsZero() {
				downSince = time.Now()
			}
		case resp.GetVersion() == target && resp.GetReady():
			downFor := ""
			if !downSince.IsZero() {
				downFor = fmt.Sprintf(" (was down %ds)", int(time.Since(downSince).Seconds()))
			}
			fmt.Printf("  server: %s answering and healthy%s\n", target, downFor)
			return nil
		default:
			lastVersion, lastReady = resp.GetVersion(), resp.GetReady()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return diagnoseWatchTimeout(api, fromVersion, target, lastVersion, lastReady)
}

// diagnoseWatchTimeout names the state at the deadline. The four cases are
// genuinely different nights for the operator; reporting only "rolled
// back" would be wrong in three of them.
func diagnoseWatchTimeout(api *client.Client, fromVersion, target, lastVersion string, lastReady bool) error {
	upgradeEvent := latestUpgradeEvent(api)
	host := "the host"

	switch {
	case lastVersion == target && !lastReady:
		return fmt.Errorf("server is running %s but has not finished starting after %s — it may be crash-looping; on %s check: journalctl -u runed -n 50", target, upgradeWatchDeadline, host)
	case lastVersion == fromVersion && strings.Contains(upgradeEvent, "rolled-back"):
		return fmt.Errorf("server rolled back to %s — see: journalctl -u runed-upgrade on %s\n(%s)", fromVersion, host, upgradeEvent)
	case lastVersion == fromVersion && upgradeEvent == "":
		return fmt.Errorf("server still reports %s and no upgrade activity was recorded — the staged upgrade may never have been applied; on %s check: systemctl status runed-upgrade.path runed-upgrade.service", fromVersion, host)
	case lastVersion == fromVersion:
		return fmt.Errorf("server still reports %s (%s) — see journalctl -u runed-upgrade on %s", fromVersion, upgradeEvent, host)
	default:
		return fmt.Errorf("server did not answer within %s — it may still be mid-rollback; on %s check: journalctl -u runed -u runed-upgrade -n 80", upgradeWatchDeadline, host)
	}
}

// latestUpgradeEvent best-effort fetches the most recent Upgrade* node
// event ("" when none or unreadable).
func latestUpgradeEvent(api *client.Client) string {
	ec := generated.NewEventServiceClient(api.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := ec.ListEvents(ctx, &generated.ListEventsRequest{Limit: 50})
	if err != nil {
		return ""
	}
	for _, e := range resp.GetEvents() {
		if strings.HasPrefix(e.GetReason(), "Upgrade") {
			return e.GetReason() + ": " + e.GetMessage()
		}
	}
	return ""
}

// postRestartSweep lists instances that did not come back cleanly and
// prints the exact re-arm command for each — a runed restart is known to
// strand volume-backed instances in VolumeNotReady occasionally, and they
// need `rune restart` rather than patience.
func postRestartSweep(api *client.Client) {
	ic := generated.NewInstanceServiceClient(api.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := ic.ListInstances(ctx, &generated.ListInstancesRequest{Namespace: "*"})
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, inst := range resp.GetInstances() {
		stuck := inst.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_FAILED ||
			strings.Contains(inst.GetStatusMessage(), "VolumeNotReady") ||
			strings.Contains(inst.GetStatusMessage(), "Stalled")
		if !stuck {
			continue
		}
		key := inst.GetNamespace() + "/" + inst.GetServiceName()
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Printf("  note: %s is not running (%s) — re-arm it with: rune restart %s -n %s\n",
			key, inst.GetStatusMessage(), inst.GetServiceName(), inst.GetNamespace())
	}
}

func upgradeClient(ctx context.Context, hc *http.Client, checksums upgrade.Checksums, target string) error {
	digest, err := checksums.Digest(upgrade.CLIAsset(runtime.GOOS, runtime.GOARCH))
	if err != nil {
		return err
	}
	path, err := upgrade.SelfUpdate(ctx, hc, target, digest)
	var notWritable *upgrade.ErrBinaryNotWritable
	if errors.As(err, &notWritable) {
		return fmt.Errorf("cannot replace %s (installed by root?) — run:  sudo rune upgrade --client %s", path, target)
	}
	if err != nil {
		return err
	}
	fmt.Printf("  client: %s → %s (sha256 verified)\n", path, target)
	return nil
}

func confirmDowngrade(from, target string, yes bool, allowDowngrade *bool) error {
	cmp, err := upgrade.CompareVersions(target, from)
	if err != nil {
		// The server's version may be unparseable (a from-source build);
		// treat as not-a-downgrade and let the host-side floor decide.
		return nil
	}
	if cmp >= 0 {
		return nil
	}
	*allowDowngrade = true
	fmt.Printf("⚠️  %s is OLDER than the server's %s — this is a downgrade.\n", target, from)
	if yes {
		return nil
	}
	if !confirmPrompt("Downgrade the server?") {
		return fmt.Errorf("aborted")
	}
	return nil
}

func confirmPrompt(q string) bool {
	fmt.Printf("%s [y/N] ", q)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(sc.Text()))
	return answer == "y" || answer == "yes"
}

func currentContextName() string {
	if n := viper.GetString("current-context"); n != "" {
		return n
	}
	return "default"
}
