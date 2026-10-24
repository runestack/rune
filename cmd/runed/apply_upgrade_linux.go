//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/runestack/rune/pkg/upgrade"
	"github.com/runestack/rune/pkg/version"
)

// runApplyUpgrade is the root applier entry point (RUNE-321), executed by
// the runed-upgrade oneshot when the path unit sees a staged `ready`
// trigger. It must run as root: it swaps root-owned binaries and drives
// systemd. Everything it reads from the staging dir is untrusted — see
// pkg/upgrade.Applier.
func runApplyUpgrade(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply-upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		staging    = fs.String("staging", "", "Staging directory runed writes to (<data-dir>/upgrade)")
		configPath = fs.String("config", "", "Runefile path, for the post-restart verify address (empty = defaults)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *staging == "" {
		fmt.Fprintln(stderr, "runed apply-upgrade: --staging is required")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "runed apply-upgrade: must run as root (it swaps root-owned binaries and restarts runed)")
		return 1
	}

	a := &upgrade.Applier{
		StagingDir: *staging,
		ConfigPath: *configPath,
		Runtime: upgrade.ApplierRuntime{
			CurrentVersion: version.Version,
			FloorPath:      upgrade.FloorPath,
		},
	}
	if err := a.Apply(context.Background()); err != nil {
		fmt.Fprintf(stderr, "runed apply-upgrade: %v\n", err)
		return 1
	}
	_ = stdout
	return 0
}
