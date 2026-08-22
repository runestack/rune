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
