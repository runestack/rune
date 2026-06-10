//go:build e2e
// +build e2e

package harness

import (
	"fmt"
	"strings"
)

// Options controls the generated runefile and server flags. The zero
// value is a quiet, fully isolated dev-mode server: badger store in a
// temp dir, no metrics, no observability persistence, no secret
// encryption.
type Options struct {
	// LogLevel for runed. Defaults to "debug" so failure dumps carry
	// maximum signal; the log only surfaces when a test fails.
	LogLevel string

	// ObservabilityBackend enables RuneSight persistence when set
	// (e.g. "embedded"). Empty leaves observability disabled — live
	// streams only.
	ObservabilityBackend string

	// Metrics enables the Prometheus endpoint on a dynamic port,
	// exposed via Context.MetricsAddr.
	Metrics bool

	// RunefileExtra is appended verbatim to the generated
	// runefile.toml for knobs the harness has no first-class option
	// for. Sections must not collide with the generated ones.
	RunefileExtra string
}

// Option mutates Options.
type Option func(*Options)

// WithObservability enables RuneSight persistence with the given
// backend ("embedded" needs no external services).
func WithObservability(backend string) Option {
	return func(o *Options) { o.ObservabilityBackend = backend }
}

// WithMetrics enables the Prometheus /metrics endpoint.
func WithMetrics() Option {
	return func(o *Options) { o.Metrics = true }
}

// WithLogLevel overrides the server log level.
func WithLogLevel(level string) Option {
	return func(o *Options) { o.LogLevel = level }
}

// WithRunefileExtra appends raw TOML to the generated runefile.
func WithRunefileExtra(toml string) Option {
	return func(o *Options) { o.RunefileExtra = toml }
}

// renderRunefile produces the per-test runefile.toml.
func renderRunefile(o Options, dataDir, grpcAddr, httpAddr, metricsAddr string) string {
	level := o.LogLevel
	if level == "" {
		level = "debug"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "data_dir = %q\n\n", dataDir)
	fmt.Fprintf(&b, "[server]\ngrpc_address = %q\nhttp_address = %q\n\n", grpcAddr, httpAddr)
	fmt.Fprintf(&b, "[log]\nlevel = %q\nformat = \"json\"\n\n", level)
	// dev_mode skips nftables and binds user ports; cluster_cidr only
	// seeds store-level VIP state, so a shared default is fine across
	// concurrent instances.
	b.WriteString("[networking]\ndev_mode = true\ncluster_cidr = \"10.96.0.0/16\"\n\n")
	fmt.Fprintf(&b, "[telemetry]\nmetrics_addr = %q\n\n", metricsAddr)
	// Key encryption needs a KEK source; tests don't exercise it.
	b.WriteString("[secret.encryption]\nenabled = false\n\n")
	if o.ObservabilityBackend != "" {
		fmt.Fprintf(&b, "[observability]\nenabled = true\nbackend = %q\n\n", o.ObservabilityBackend)
	} else {
		b.WriteString("[observability]\nenabled = false\n\n")
	}
	if o.RunefileExtra != "" {
		b.WriteString(o.RunefileExtra)
		b.WriteString("\n")
	}
	return b.String()
}
