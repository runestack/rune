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
	"sync"
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

// Timing. The stage RPC must outlive the server's ~4-minute CDN-504 retry
// budget: the default 30s call timeout would cancel the download before the
// retry ever ran. The watch deadline is derived from the applier's worst
// case — checksum fetch (up to 4m) + swap + verify (3m) + hold-down + a
// rollback restart and its verify (1m) — with margin. Both are deliberately
// constants, not flags: on expiry the output names what to check.
const (
	upgradeStageTimeout  = 6 * time.Minute
	upgradeWatchDeadline = 12 * time.Minute
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
failed server upgrade does not leave a newer CLI against an older server.
Passing a version older than the running one is a downgrade and asks for
explicit confirmation; the server host additionally enforces a root-owned
version floor.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 && args[0] != "latest" {
				target = args[0]
				// Catch the one plausible misreading at the moment it
				// happens: someone reaching for `rune upgrade <service>`.
				if _, err := upgrade.ParseVersion(target); err != nil {
					return fmt.Errorf("%q is not a release version — `rune upgrade` updates Rune itself (server and CLI); to update a service, use `rune cast`", target)
				}
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
	if (!plan.doServer || plan.serverAtTarget) && !plan.clientAtTarget {
		// A downgrade that only touches the client (no server half, or
		// the server already at target) deserves the same pause.
		var ignored bool
		if err := confirmDowngrade(version.Version, plan.target, opts.yes, &ignored); err != nil {
			return err
		}
	}
	if plan.doServer && !plan.serverAtTarget {
		if err := confirmDowngrade(plan.server.GetVersion(), plan.target, opts.yes, &plan.allowDowngrade); err != nil {
			return err
		}
		fmt.Println("Ingress and service-to-service traffic pause while the server restarts —")
		fmt.Println("typically ~15s, up to a few minutes if the upgrade rolls back.")
		fmt.Println("Container instances keep running; process-mode services restart with it.")
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
	serverUpgraded := !plan.doServer || plan.serverAtTarget
	var serverSkipped *degradeSkipped
	var seq eventBaseline
	switch {
	case plan.doServer && !plan.serverAtTarget:
		proceeded, skipped, staged, err := upgradeServer(ctx, api, plan.server, plan.checksums, plan.target, plan.allowDowngrade, opts)
		if err != nil {
			return err
		}
		serverUpgraded = proceeded
		serverSkipped = skipped
		seq = staged
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
	if !plan.clientAtTarget {
		if err := upgradeClient(ctx, hc, plan.checksums, plan.target); err != nil {
			return err
		}
	} else {
		fmt.Printf("  client: already at %s\n", plan.target)
	}
	// Never report more than happened. The partial result gets no ✓, and
	// says the fact once — as the error when someone can act on it.
	if serverUpgraded && plan.doServer {
		reportUnitLeftUnchanged(api, seq)
	}
	if !serverUpgraded {
		summary := fmt.Sprintf("CLI upgraded to %s; server still on %s.", plan.target, plan.server.GetVersion())
		if serverSkipped != nil && !serverSkipped.actionable {
			fmt.Println(summary)
			return nil
		}
		return fmt.Errorf("%s", summary)
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
func upgradeServer(ctx context.Context, api *client.Client, server *generated.GetServerVersionResponse, checksums upgrade.Checksums, target string, allowDowngrade bool, opts *upgradeOptions) (bool, *degradeSkipped, eventBaseline, error) {
	if server.GetOs() != "linux" {
		proceed, _, skipped, hard := degrade(degradeSkipped{
			reason: fmt.Sprintf("it runs %s/%s; servers self-upgrade on linux only", server.GetOs(), server.GetArch()),
		}, opts)
		return proceed, skipped, eventBaseline{}, hard
	}
	digest, err := checksums.Digest(upgrade.ServerAsset(server.GetArch()))
	if err != nil {
		return false, nil, eventBaseline{}, err
	}

	fmt.Printf("  server: staging %s ...\n", target)
	seqBaseline := eventSeqBaseline(api)
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
		proceed, watchAnyway, skipped, herr := classifyStageError(err, target, opts)
		if herr != nil {
			return false, nil, seqBaseline, herr
		}
		if !proceed {
			return false, skipped, seqBaseline, nil // degraded to client-only; message already printed
		}
		stagedUncertain = watchAnyway
	}
	if !stagedUncertain {
		fmt.Println("  server: staged (sha256 verified)")
	}
	fmt.Println("  server: applying — connection will drop while runed restarts")
	fmt.Println("          Ctrl-C is safe: the upgrade finishes or rolls back on the host")

	return true, nil, seqBaseline, watchServerUpgrade(ctx, api, server.GetVersion(), target, seqBaseline)
}

// classifyStageError maps an UpgradeServer failure to what to do next:
// proceed+watch (the restart may have raced the reply), degrade to a
// client-only upgrade with an explanation, or fail hard.
func classifyStageError(err error, target string, opts *upgradeOptions) (proceed, watchAnyway bool, skipped *degradeSkipped, hard error) {
	st, ok := status.FromError(err)
	if !ok {
		return true, true, nil, nil
	}
	// Sanitise once, here, rather than at each site that prints it: this
	// text crosses a channel with no transport authentication, and the
	// per-site approach already grew a sixth unsanitised site a round
	// after five were counted.
	msg := upgrade.SanitizeServerDetail(st.Message())
	sshOneLiner := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/%s/scripts/upgrade-server.sh | sudo bash -s -- --version %s --refresh-unit",
		upgrade.Repo, target, target)
	if dir := upgrade.DataDirFromMessage(msg); dir != "" {
		// The server's data dir is somewhere other than the default, so
		// the remedy has to install units that watch it.
		sshOneLiner += " --data-dir " + dir
	}

	switch st.Code() {
	case codes.Unauthenticated:
		return degrade(degradeSkipped{reason: "your session has expired", nextStep: "run `rune login`, then retry", actionable: true}, opts)
	case codes.PermissionDenied:
		return degrade(degradeSkipped{reason: "you need an admin token"}, opts)
	case codes.Unimplemented:
		return degrade(degradeSkipped{reason: "this server is too old to upgrade itself",
			nextStep: "run this on the host once:\n" + sshOneLiner, actionable: true}, opts)
	case codes.FailedPrecondition:
		switch reason := upgrade.PreconditionReason(msg); reason {
		case upgrade.ReasonInProgress:
			return false, false, nil, fmt.Errorf("an upgrade is already in progress on the server: %s", msg)
		case upgrade.ReasonNoSystemd:
			return degrade(degradeSkipped{reason: "it isn't running under systemd, so it can't restart itself"}, opts)
		case upgrade.ReasonUnitsMissing:
			why := "its upgrade helper isn't installed yet"
			// The generic "X not installed" adds nothing; a mismatched
			// staging path is the variant worth quoting.
			if d := msg; strings.Contains(d, "stages to") {
				// Just the mismatch, not the whole message — the remedy
				// below already says to reinstall, and the one-liner
				// already carries the right --data-dir.
				if w, s2, ok := strings.Cut(d, " watches "); ok {
					_ = w
					if watched, staged, ok2 := strings.Cut(s2, ", but this server stages to "); ok2 {
						staged = strings.TrimSuffix(strings.TrimSpace(staged), " — reinstall the units")
						why += " (it watches " + strings.TrimSpace(watched) + ", not " + staged + ")"
					}
				}
			}
			return degrade(degradeSkipped{reason: why,
				nextStep: "run this on the host once:\n" + sshOneLiner, actionable: true}, opts)
		default:
			return false, false, nil, fmt.Errorf("server refused the upgrade: %s", msg)
		}
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		// The reply may have raced the restart: treat as possibly staged
		// and watch rather than reporting failure for a success.
		fmt.Println("  server: connection dropped during staging — watching for the new version anyway")
		return true, true, nil, nil
	default:
		return false, false, nil, fmt.Errorf("staging upgrade: %s", msg)
	}
}

// degradeSkipped records why the server half did not run, and whether
// anyone can do something about it. A developer without an admin token has
// nothing to act on, so that path must not exit non-zero — an exit code
// that always fires is an exit code people learn to ignore.
type degradeSkipped struct {
	reason     string
	nextStep   string
	actionable bool
}

// degrade prints the one-line reason (and its next step, if any) and
// records it for the summary. The `client:` line that follows proves the
// CLI half still ran, so the reason never repeats it.
func degrade(d degradeSkipped, opts *upgradeOptions) (bool, bool, *degradeSkipped, error) {
	if opts.serverOnly {
		if d.nextStep != "" {
			return false, false, nil, fmt.Errorf("%s\n\n  %s", d.reason, strings.ReplaceAll(d.nextStep, "\n", "\n  "))
		}
		return false, false, nil, fmt.Errorf("%s", d.reason)
	}
	fmt.Printf("  server: not upgraded — %s\n", d.reason)
	for _, line := range strings.Split(d.nextStep, "\n") {
		if line != "" {
			fmt.Printf("          %s\n", line)
		}
	}
	return false, false, &d, nil
}

// eventSeqBaseline records the newest event's sequence and timestamp before
// staging, so the watch can tell this attempt's outcome from a previous one
// without comparing a server timestamp to the local clock.
func eventSeqBaseline(api *client.Client) eventBaseline {
	ec := generated.NewEventServiceClient(api.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := ec.ListEvents(ctx, &generated.ListEventsRequest{Limit: 1})
	if err != nil || len(resp.GetEvents()) == 0 {
		return eventBaseline{}
	}
	return eventBaseline{seq: resp.GetEvents()[0].GetSeq(), lastSeen: resp.GetEvents()[0].GetLastSeen()}
}

// eventBaseline is the newest event before staging, by both of the server's
// own orderings. Sequence alone is not enough: the recorder folds a repeat
// of an identical failure onto its ORIGINAL sequence, advancing only Count
// and LastSeen — so a repeat with no intervening Node event would look older
// than the baseline and be skipped, the misdiagnosis this replaced a clock
// comparison to avoid.
type eventBaseline struct {
	seq      int64
	lastSeen string
}

// newerThan reports whether an event is from this attempt.
func (b eventBaseline) newerThan(seq int64, lastSeen string) bool {
	if seq > b.seq {
		return true
	}
	return b.lastSeen != "" && lastSeen > b.lastSeen
}

// watchServerUpgrade polls the public GetServerVersion through the restart
// window until the target version answers ready, then reports. On the
// deadline it distinguishes the terminal states instead of guessing.
func watchServerUpgrade(ctx context.Context, api *client.Client, fromVersion, target string, since eventBaseline) error {
	hc := generated.NewHealthServiceClient(api.Conn())
	deadline := time.Now().Add(upgradeWatchDeadline)
	var downSince time.Time
	lastVersion, lastReady := "", false

	requireReady := watchRequiresReady(target, fromVersion)

	for poll := 0; time.Now().Before(deadline); poll++ {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := hc.GetServerVersion(pctx, &generated.GetServerVersionRequest{})
		cancel()
		switch {
		case err != nil:
			if downSince.IsZero() {
				downSince = time.Now()
			}
			// The only state that can reach the full deadline in silence.
			if poll%15 == 14 {
				fmt.Printf("  server: still down (%s)\n", time.Since(downSince).Round(time.Second))
			}
		case resp.GetVersion() == target && (resp.GetReady() || !requireReady):
			printWatchSuccess(target, downSince)
			return nil
		default:
			lastVersion, lastReady = resp.GetVersion(), resp.GetReady()
			if done := earlyOutcome(api, since, poll, fromVersion, target, lastVersion, lastReady); done != nil {
				return done
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return diagnoseWatchTimeout(api, since, fromVersion, target, lastVersion, lastReady)
}

// earlyOutcome ends the wait as soon as this attempt's outcome is on the
// record: a floor refusal or a verification failure lands as an event within
// seconds, and holding the full deadline to report it would be a long stare
// at a known answer. Returns nil while the outcome is still open.
func earlyOutcome(api *client.Client, since eventBaseline, poll int, fromVersion, target, lastVersion string, lastReady bool) error {
	if poll%5 != 4 || lastVersion != fromVersion {
		return nil
	}
	ev := latestTerminalUpgradeEvent(api, since)
	if ev == "" {
		return nil
	}
	return watchTimeoutDiagnosis(currentContextName(), fromVersion, target, lastVersion, lastReady, ev)
}

// reportUnitLeftUnchanged prints the applier's refusal to touch this host's
// unit, if there was one. A refused refresh leaves the unit frozen relative
// to the binary — every later release's unit directives pass it by — and
// nothing else on a successful run would tell the operator.
//
// Read here rather than at the success line because the applier records it
// only after its hold-down, roughly ten seconds after the CLI can first see
// the server healthy. By this point the client half has downloaded and
// verified its own tarball, so the event has long since landed and no wait
// was added to get it.
func reportUnitLeftUnchanged(api *client.Client, since eventBaseline) {
	note := unitUnchangedNote(api, since)
	if note == "" {
		return
	}
	fmt.Printf("\nnote: %s\n", note)
	fmt.Println("      it will not pick up new unit directives until you reconcile it.")
}

// watchRequiresReady mirrors the applier: a downgrade target may predate the
// ready flag, so the watch demands it only in the upgrade direction.
func watchRequiresReady(target, fromVersion string) bool {
	cmp, err := upgrade.CompareVersions(target, fromVersion)
	return err != nil || cmp >= 0
}

func printWatchSuccess(target string, downSince time.Time) {
	downFor := ""
	if !downSince.IsZero() {
		downFor = fmt.Sprintf(" (was down %ds)", int(time.Since(downSince).Seconds()))
	}
	fmt.Printf("  server: %s answering and healthy%s\n", target, downFor)
}

// diagnoseWatchTimeout names the state at the deadline. The four cases are
// genuinely different nights for the operator; reporting only "rolled
// back" would be wrong in three of them.
func diagnoseWatchTimeout(api *client.Client, since eventBaseline, fromVersion, target, lastVersion string, lastReady bool) error {
	return watchTimeoutDiagnosis(currentContextName(), fromVersion, target, lastVersion, lastReady, latestTerminalUpgradeEvent(api, since))
}

// watchTimeoutDiagnosis is the pure decision; terminalEvent is the most
// recent APPLY-outcome event ("" when none). UpgradeStaged is deliberately
// not a terminal event — the stager emits it before creating the trigger,
// so it exists on every attempt and would make "never applied"
// undetectable.
func watchTimeoutDiagnosis(host, fromVersion, target, lastVersion string, lastReady bool, terminalEvent string) error {
	switch {
	case lastVersion == target && !lastReady:
		return fmt.Errorf("server is running %s but has not finished starting after %s — it may be crash-looping; on %s check: journalctl -u runed -n 50", target, upgradeWatchDeadline, host)
	case lastVersion == fromVersion && strings.Contains(terminalEvent, "rolled-back"):
		return fmt.Errorf("server rolled back to %s — see: journalctl -u runed-upgrade on %s\n(%s)", fromVersion, host, terminalEvent)
	case lastVersion == fromVersion && terminalEvent == "":
		return fmt.Errorf("server still reports %s and no apply outcome was recorded — on %s check: journalctl -u runed-upgrade -n 80, then systemctl status runed-upgrade.path", fromVersion, host)
	case lastVersion == fromVersion:
		return fmt.Errorf("server still reports %s (%s) — see journalctl -u runed-upgrade on %s", fromVersion, terminalEvent, host)
	default:
		return fmt.Errorf("server did not answer within %s — it may still be mid-rollback; on %s check: journalctl -u runed -u runed-upgrade -n 80", upgradeWatchDeadline, host)
	}
}

// unitUnchangedNote returns the applier's declined-refresh message, matched
// on its own event reason rather than on the wording of the message.
func unitUnchangedNote(api *client.Client, since eventBaseline) string {
	ec := generated.NewEventServiceClient(api.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := ec.ListEvents(ctx, &generated.ListEventsRequest{Limit: 50})
	if err != nil {
		return ""
	}
	for _, e := range resp.GetEvents() {
		if e.GetReason() != "UpgradeAppliedUnitUnchanged" {
			continue
		}
		if !since.newerThan(e.GetSeq(), e.GetLastSeen()) {
			continue
		}
		msg := upgrade.SanitizeServerDetail(e.GetMessage())
		if _, note, ok := strings.Cut(msg, "("); ok {
			return strings.TrimSuffix(note, ")")
		}
		return msg
	}
	return ""
}

// latestTerminalUpgradeEvent returns this attempt's apply outcome ("" when
// none yet). The window is generous because a restart is the noisiest
// moment on the box — every instance reconcile emits — and the outcome
// must not be crowded out by them.
func latestTerminalUpgradeEvent(api *client.Client, since eventBaseline) string {
	ec := generated.NewEventServiceClient(api.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := ec.ListEvents(ctx, &generated.ListEventsRequest{Limit: 50})
	if err != nil {
		return ""
	}
	for _, e := range resp.GetEvents() {
		switch e.GetReason() {
		case "UpgradeApplied", "UpgradeAppliedUnitUnchanged", "UpgradeRolledBack", "UpgradeFailed", "UpgradeSkipped":
			// Skip a previous attempt's outcome; see eventBaseline for why
			// sequence alone is not enough.
			if !since.newerThan(e.GetSeq(), e.GetLastSeen()) {
				continue
			}
			return upgrade.SanitizeServerDetail(e.GetReason() + ": " + e.GetMessage())
		}
	}
	return ""
}

// postRestartSweep names the services that did not come back after the
// restart and prints the exact command to re-arm each. A runed restart can
// leave a volume-backed service stranded (VolumeNotReady) where it needs a
// deliberate `rune restart` rather than patience.
//
// It reads Service, not Instance: only Service carries status_reason, the
// machine-friendly slug. Instance has status_message (prose) but no slug to
// match on.
func postRestartSweep(api *client.Client) {
	sc := generated.NewServiceServiceClient(api.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := sc.ListServices(ctx, &generated.ListServicesRequest{Namespace: "*"})
	if err != nil {
		return
	}
	for _, svc := range resp.GetServices() {
		// Failed only: a service still Pending seconds after the restart is
		// converging, not stranded, and telling the operator to restart it
		// is wrong. Stalled instances roll up to a Failed service, which is
		// how the volume case this exists for surfaces.
		if svc.GetStatus() != generated.ServiceStatus_SERVICE_STATUS_FAILED {
			continue
		}
		why := svc.GetStatusReason()
		if why == "" {
			why = svc.GetStatus().String()
		}
		fmt.Printf("  note: %s/%s is not running (%s) — re-arm it with: rune restart %s -n %s\n",
			svc.GetNamespace(), svc.GetName(), why, svc.GetName(), svc.GetNamespace())
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
	fmt.Printf("⚠️  %s is OLDER than the current %s — this is a downgrade.\n", target, from)
	if yes {
		return nil
	}
	if !confirmPrompt("Downgrade?") {
		return fmt.Errorf("aborted")
	}
	return nil
}

var (
	stdinScanner     *bufio.Scanner
	stdinScannerOnce sync.Once
)

func confirmPrompt(q string) bool {
	fmt.Printf("%s [y/N] ", q)
	stdinScannerOnce.Do(func() { stdinScanner = bufio.NewScanner(os.Stdin) })
	if !stdinScanner.Scan() {
		fmt.Println("\nnot a terminal — re-run with --yes to confirm non-interactively")
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(stdinScanner.Text()))
	return answer == "y" || answer == "yes"
}

func currentContextName() string {
	if n := viper.GetString("current-context"); n != "" {
		return n
	}
	return "default"
}
