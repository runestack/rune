package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/runestack/rune/pkg/systemd"
)

// runPrintSystemd handles `runed print-systemd`. Renders the canonical
// systemd unit (pkg/systemd) to stdout with the user/group/binary/config
// fields interpolated. Operator-facing: consumed by
// `scripts/upgrade-server.sh --refresh-unit` to drop a current unit
// onto a host whose on-disk runed.service has drifted from the
// installed binary's expected shape.
//
// Named `print-systemd` (not `print-unit`) so the init system family is
// explicit in the command name. Future ports to launchd / OpenRC /
// Windows Service Manager get sibling commands (`print-launchd` etc.)
// rather than overloading a single generic name.
//
// Exits non-zero on flag-parse or render error so callers can rely on
// `set -e` semantics in upgrade-server.sh.
func runPrintSystemd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("print-systemd", flag.ContinueOnError)
	fs.SetOutput(stderr)

	defaults := systemd.DefaultUnitOptions()
	var (
		user       = fs.String("user", defaults.User, "Linux user runed runs as")
		group      = fs.String("group", defaults.Group, "Linux group runed runs as (usually matches --user)")
		binaryPath = fs.String("binary", defaults.BinaryPath, "Absolute path to the runed binary on the host")
		configPath = fs.String("config", defaults.ConfigPath, "Path passed to runed via --config (empty omits the flag and lets runed auto-discover)")

		upgradeUnits    = fs.Bool("upgrade-units", false, "Render runed-upgrade.service (the root applier oneshot) instead of runed.service")
		upgradePathUnit = fs.Bool("upgrade-path-unit", false, "Render runed-upgrade.path (watches the staging dir for a staged upgrade) instead of runed.service")
		stagingDir      = fs.String("staging", "", "Staging directory the upgrade units act on, normally <data-dir>/upgrade (required with --upgrade-units / --upgrade-path-unit)")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: runed print-systemd [flags]

Renders the canonical systemd unit for this runed binary to stdout.

The unit is versioned with runed, so 'runed print-systemd' from a given
binary always emits the unit shape that binary expects. Used by
'scripts/upgrade-server.sh --refresh-unit' to refresh
/etc/systemd/system/runed.service on hosts whose on-disk unit has
drifted from current.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, `
Example:
  runed print-systemd > /etc/systemd/system/runed.service
  systemctl daemon-reload
  systemctl restart runed
`)
	}

	if err := fs.Parse(args); err != nil {
		// flag has already printed the error via fs.Output()/Usage.
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "runed print-systemd: unexpected positional argument(s): %v\n", fs.Args())
		return 2
	}

	if *upgradeUnits || *upgradePathUnit {
		uo := systemd.UpgradeUnitOptions{
			StagingDir: *stagingDir,
			BinaryPath: *binaryPath,
			ConfigPath: *configPath,
		}
		render := systemd.RenderUpgradeService
		if *upgradePathUnit {
			render = systemd.RenderUpgradePath
		}
		out, err := render(uo)
		if err != nil {
			fmt.Fprintf(stderr, "runed print-systemd: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, out)
		return 0
	}

	opts := systemd.UnitOptions{
		User:       *user,
		Group:      *group,
		BinaryPath: *binaryPath,
		ConfigPath: *configPath,
	}
	if _, err := systemd.Render(stdout, opts); err != nil {
		fmt.Fprintf(stderr, "runed print-systemd: %v\n", err)
		return 1
	}
	return 0
}

// dispatchSubcommand inspects os.Args for a recognised subcommand
// before flag.Parse() runs. Returns (handled=true, exitCode) for
// recognised subcommands; (handled=false, 0) for the normal runed
// daemon path. Keeping subcommand handling here means the daemon's
// flag set (the `var (...)` block in main.go) doesn't need to know
// these helper modes exist.
func dispatchSubcommand() (handled bool, exitCode int) {
	if len(os.Args) < 2 {
		return false, 0
	}
	switch os.Args[1] {
	case "print-systemd":
		return true, runPrintSystemd(os.Args[2:], os.Stdout, os.Stderr)
	case "apply-upgrade":
		return true, runApplyUpgrade(os.Args[2:], os.Stdout, os.Stderr)
	default:
		// Falls through to the daemon path; main() rejects unknown
		// positional args.
		return false, 0
	}
}
