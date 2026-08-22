package main

import (
	"os"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/version"
	"github.com/spf13/viper"
)

// mustInitRuntime runs startup phases 1-2 (RUNE-313): runtime config, logger,
// signal context, and the global viper bind. It also creates the closer stack
// that every later phase pushes onto.
//
// Body moved verbatim from main(); locals are unpacked/packed at the edges so
// the move stays reviewable line-for-line.
func mustInitRuntime() *boot {
	initRuntimeConfig()

	// Build logger using helper
	logger := buildLogger(*logLevel, *logFormat, *prettyLogs, *debugLogLevel)

	logger.Info("Starting Rune Server", log.Str("version", version.Version))

	// Context with cancellation
	ctx, cancel := setupSignalContext(logger)

	// Teardown order, explicit and reversed at the end of main (RUNE-313).
	// Pushes sit exactly where the matching `defer` used to.
	var closers closerStack
	closers.push("signal-context", cancel)

	// Bind the global viper to the same runefile initRuntimeConfig
	// resolved. Without a runefile we fail fast — production deployments
	// must ship one (see docs); the dev-loop just needs `runefile.toml`
	// in the cwd. Tests use the override hook in initRuntimeConfig.
	resolvedRunefile := resolveRunefilePath()
	if resolvedRunefile == "" {
		logger.Error("No runefile found; pass --config or place runefile.{toml,yaml,yml} in cwd or /etc/rune/")
		os.Exit(1)
	}
	viper.SetConfigFile(resolvedRunefile)
	if err := viper.ReadInConfig(); err != nil {
		logger.Error("Failed to read runefile", log.Str("path", resolvedRunefile), log.Err(err))
		os.Exit(1)
	}

	return &boot{ctx: ctx, logger: logger, runefile: resolvedRunefile, closers: &closers}
}
