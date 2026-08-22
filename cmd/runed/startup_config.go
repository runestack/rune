package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

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

func buildLogger(levelStr, formatStr string, pretty, debug bool) log.Logger {
	if pretty {
		formatStr = "text"
	}
	if debug {
		levelStr = "debug"
	}
	var opts []log.LoggerOption
	lvl, err := log.ParseLevel(levelStr)
	if err != nil {
		fmt.Printf("Invalid log level: %s, defaulting to 'info'\n", levelStr)
		lvl = log.InfoLevel
	}
	opts = append(opts, log.WithLevel(lvl))
	switch strings.ToLower(formatStr) {
	case "json":
		opts = append(opts, log.WithFormatter(&log.JSONFormatter{}))
	case "text", "pretty":
		opts = append(opts, log.WithFormatter(&log.TextFormatter{}))
	default:
		fmt.Printf("Invalid log format: %s, defaulting to 'text'\n", formatStr)
		opts = append(opts, log.WithFormatter(&log.TextFormatter{}))
	}
	return log.NewLogger(opts...)
}
func setupSignalContext(logger log.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal, shutting down (press Ctrl+C again to force quit)", log.Str("signal", sig.String()))
		cancel()
		sig = <-sigCh
		logger.Warn("Received second signal, forcing exit", log.Str("signal", sig.String()))
		os.Exit(1)
	}()
	return ctx, cancel
}
