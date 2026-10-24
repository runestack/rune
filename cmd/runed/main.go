package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/runestack/rune/cmd/runed/startup"
	"github.com/runestack/rune/pkg/version"
)

func main() {
	// Helper subcommands (e.g. `runed print-unit`) short-circuit the
	// daemon flag parser so they can own their own flag set without
	// polluting the daemon's flag surface. See print_systemd.go.
	if handled, code := dispatchSubcommand(); handled {
		os.Exit(code)
	}

	f := startup.DefineFlags(flag.CommandLine)
	flag.Parse()

	// A positional arg here is a subcommand this binary doesn't know
	// (flag.Parse just parks it in flag.Args()). Falling through would
	// launch the daemon — as root, when the caller is the upgrade
	// oneshot invoking `apply-upgrade` on a too-old binary — so refuse.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "runed: unknown subcommand %q\n", flag.Arg(0))
		os.Exit(2)
	}

	if f.ShowHelp {
		flag.Usage()
		return
	}
	if f.ShowVersion {
		fmt.Println(version.Info())
		return
	}

	startup.Run(f)
}
