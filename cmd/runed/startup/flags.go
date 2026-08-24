package startup

import "flag"

// Flags is the daemon's command-line surface.
//
// It is also the carrier for the *effective* configuration: resolveRuntimeConfig
// writes back into these fields for every flag the operator did not set
// explicitly, folding in runefile and environment values at the documented
// precedence (flag > env > runefile > default). So a phase reading f.DevMode
// gets the resolved value, not the raw flag default.
//
// The package owns the flag definitions rather than main, so that the
// declaration sits next to the code that consumes it and no phase has to reach
// back into package main for a global. It also means the test helper that
// resets flag state can re-register via DefineFlags instead of hand-mirroring
// a var block, which is how the old helper had already drifted out of date.
type Flags struct {
	ConfigFile string
	GRPCAddr   string
	HTTPAddr   string
	DataDir    string

	LogLevel   string
	Debug      bool
	LogFormat  string
	PrettyLogs bool

	DevMode     bool
	ClusterCIDR string
	MetricsAddr string

	NodeName     string
	GPUProvider  string
	NodeRole     string
	IngressHTTP  string
	IngressHTTPS string

	ACMEDirectory string
	ACMEEmail     string

	UIEnabled    bool
	UIPath       string
	UIRequireTLS bool

	ShowHelp    bool
	ShowVersion bool
}

// DefineFlags registers the daemon's flags on fs and returns the struct they
// write into. Call before fs.Parse.
func DefineFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	fs.StringVar(&f.ConfigFile, "config", "", "Path to runefile.yaml (server configuration)")
	fs.StringVar(&f.GRPCAddr, "grpc-addr", ":7863", "gRPC server address")
	fs.StringVar(&f.HTTPAddr, "http-addr", ":7861", "HTTP server address")
	fs.StringVar(&f.DataDir, "data-dir", "", "Data directory (if not specified, uses OS-specific application data directory)")
	fs.StringVar(&f.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	fs.BoolVar(&f.Debug, "debug", false, "Enable debug mode (shorthand for --log-level=debug)")
	fs.StringVar(&f.LogFormat, "log-format", "text", "Log format (text, json)")
	fs.BoolVar(&f.PrettyLogs, "pretty", false, "Enable pretty text log format (shorthand for --log-format=text)")
	fs.BoolVar(&f.DevMode, "dev-mode", false, "Run in dev mode: skip nftables, bind ingress on user ports, embedded DNS resolves .rune to 127.0.0.1 (laptop development)")
	fs.StringVar(&f.ClusterCIDR, "cluster-cidr", "10.96.0.0/16", "Cluster service CIDR for VIP allocation (RFC1918 or 100.64/10)")
	fs.StringVar(&f.MetricsAddr, "metrics-addr", "127.0.0.1:9100", "Address for the Prometheus /metrics endpoint (empty disables). Exposes metrics from all subsystems (orchestrator, runners, networking, agent, DNS).")
	fs.StringVar(&f.NodeName, "node-name", "", "Node ID to mint on FIRST boot (DNS-1123 label). Defaults to the hostname, then to a random node-<hex>. Ignored once node-identity.json exists — the ID is never rewritten.")
	fs.StringVar(&f.GPUProvider, "gpu-provider", "auto", "Device inventory provider: auto (nvidia-smi when present, otherwise nothing), none (never probe), or nvidia-smi.")
	fs.StringVar(&f.NodeRole, "node-role", "", "Comma-separated node roles. 'edge' enables the ingress controller and ACME orchestrator on this node.")
	fs.StringVar(&f.IngressHTTP, "ingress-http-addr", "", "Bind address for the ingress HTTP listener. Defaults to :80 in production, :8080 in dev mode. Used only when node-role contains 'edge'.")
	fs.StringVar(&f.IngressHTTPS, "ingress-https-addr", "", "Bind address for the ingress HTTPS listener. Defaults to :443 in production, :8443 in dev mode. Used only when node-role contains 'edge'. Empty disables TLS termination.")
	fs.StringVar(&f.ACMEDirectory, "acme-directory", "", "ACME directory URL. Empty defaults to Let's Encrypt production. Use a Pebble URL for integration tests.")
	fs.StringVar(&f.ACMEEmail, "acme-email", "", "Contact email passed to the ACME provider on account registration.")
	fs.BoolVar(&f.UIEnabled, "ui", true, "Serve the embedded web dashboard and gRPC-Web transcoder on the HTTP address (RUNE-200). Use --ui=false for headless installs.")
	fs.StringVar(&f.UIPath, "ui-path", "", "Mount path for the embedded dashboard (default /ui).")
	fs.BoolVar(&f.UIRequireTLS, "ui-require-tls", true, "Refuse to serve the dashboard over plaintext on a non-loopback address; bind 127.0.0.1 only when TLS is off. Set false for dev or when TLS terminates at an ingress.")
	fs.BoolVar(&f.ShowHelp, "help", false, "Show help")
	fs.BoolVar(&f.ShowVersion, "version", false, "Show version")
	return f
}
