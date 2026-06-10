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
	// ObservabilityBackend enables RuneSight persistence when set
	// (e.g. "embedded"). Empty leaves observability disabled — live
	// streams only.
	ObservabilityBackend string

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

// WithRunefileExtra appends raw TOML to the generated runefile.
func WithRunefileExtra(toml string) Option {
	return func(o *Options) { o.RunefileExtra = toml }
}

// renderRunefile produces the per-test runefile.toml.
func renderRunefile(o Options, dataDir, grpcAddr, httpAddr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "data_dir = %q\n\n", dataDir)
	fmt.Fprintf(&b, "[server]\ngrpc_address = %q\nhttp_address = %q\n\n", grpcAddr, httpAddr)
	// debug level so a failure dump carries maximum signal; the log
	// only surfaces when a test fails.
	b.WriteString("[log]\nlevel = \"debug\"\nformat = \"json\"\n\n")
	// dev_mode skips nftables and binds user ports; cluster_cidr only
	// seeds store-level VIP state, so a shared default is fine across
	// concurrent instances.
	b.WriteString("[networking]\ndev_mode = true\ncluster_cidr = \"10.96.0.0/16\"\n\n")
	b.WriteString("[telemetry]\nmetrics_addr = \"\"\n\n")
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
