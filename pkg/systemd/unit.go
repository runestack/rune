// Package systemd renders the canonical runed.service systemd unit.
//
// The unit template lives here, in Go, so it's versioned with the
// runed binary it describes. Both `runed print-systemd` (operator-facing,
// used by upgrade-server.sh --refresh-unit) and the installer scripts
// consume the same source — there is no second template that can drift
// out of sync with the binary's expectations.
//
// Adding a new directive to the unit (e.g. MemoryHigh, OOMScoreAdjust,
// a new AmbientCapabilities entry) means editing only the constant in
// this file. Every upgrade path that uses `runed print-systemd` then
// picks it up automatically.
//
// The package is named after the init system, not "unit", because future
// ports to launchd / OpenRC / Windows Service Manager will add sibling
// packages (pkg/launchd, pkg/openrc, ...) with the same shape. Keeping
// the namespace explicit avoids overloading a generic "unit" name with
// systemd-specific semantics.
package systemd

import (
	"fmt"
	"io"
	"strings"
	"text/template"
)

// UnitOptions are the inputs the canonical unit template substitutes.
// Zero values are not valid — use DefaultUnitOptions() to get sensible
// defaults and override the fields you care about.
type UnitOptions struct {
	// User is the unprivileged Linux user runed runs as.
	User string

	// Group is runed's primary group. Usually matches User.
	Group string

	// BinaryPath is the absolute path to the runed binary on the host.
	BinaryPath string

	// ConfigPath is the runefile path passed to runed via --config.
	// Empty omits the --config flag, letting runed auto-discover.
	ConfigPath string
}

// DefaultUnitOptions returns the settings install-server.sh has used
// historically: rune:rune as the service user, /usr/local/bin/runed
// as the binary, /etc/rune/runefile.toml as the config.
func DefaultUnitOptions() UnitOptions {
	return UnitOptions{
		User:       "rune",
		Group:      "rune",
		BinaryPath: "/usr/local/bin/runed",
		ConfigPath: "/etc/rune/runefile.toml",
	}
}

// Validate returns an error if required fields are empty. Called from
// Render before any template substitution so a clear message points at
// the missing field rather than a half-rendered unit.
func (o UnitOptions) Validate() error {
	if strings.TrimSpace(o.User) == "" {
		return fmt.Errorf("systemd: UnitOptions.User is required")
	}
	if strings.TrimSpace(o.Group) == "" {
		return fmt.Errorf("systemd: UnitOptions.Group is required")
	}
	if strings.TrimSpace(o.BinaryPath) == "" {
		return fmt.Errorf("systemd: UnitOptions.BinaryPath is required")
	}
	return nil
}

// unitTemplate is the canonical runed.service shape. Comments in the
// rendered unit are intentional: they survive into the on-disk file so
// an operator running `systemctl cat runed` understands why each
// directive is there.
//
// Any directive that materially changes runed's process environment
// (capabilities, resource limits, restart policy, security knobs,
// SupplementaryGroups for runtime sockets) belongs here, not in a
// drop-in. Drop-ins are reserved for facts the installer must
// inject at install time — e.g. host-specific environment variables
// in env.conf — that genuinely don't belong in the binary-versioned
// canonical unit.
const unitTemplate = `[Unit]
Description=Rune Server
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User={{.User}}
Group={{.Group}}
# Grants access to /var/run/docker.sock without putting docker as
# the primary group of the rune user. Required for the Docker runner;
# baked in here so it survives upgrade-server.sh --refresh-unit (the
# previous workaround was a /etc/systemd/system/runed.service.d
# drop-in that operators kept losing on upgrades).
SupplementaryGroups=docker
# Ensure non-interactive stdin under systemd
StandardInput=null
# --config makes the resolved runefile path explicit and survives
# anyone running 'systemctl status runed' / 'cat /etc/systemd/system/runed.service'
# trying to figure out where config came from. If the file is absent
# runed falls through to its built-in defaults; the auto-discovery
# search order is unchanged.
ExecStart={{.BinaryPath}}{{if .ConfigPath}} --config {{.ConfigPath}}{{end}}
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
# Capabilities the agent needs while running as the rune user:
#   - CAP_NET_BIND_SERVICE — bind :80 / :443 for the ingress.
#   - CAP_SYS_ADMIN        — mount(2) for cloud block-device drivers
#     (do-volume + future) which attach a device and mount it under
#     /var/lib/rune/mounts/. Without it, mount(2) returns EPERM and
#     every cloud volume gets stuck post-Attach.
#   - CAP_CHOWN, CAP_FOWNER — applyFSOwnership chown(2) + chmod(2) on
#     the mount root when VolumeMount.fsUser/fsGroup/fsMode is set.
#     Cheap, inert when the operator hasn't opted in, and the
#     alternative is "every operator who flips on fsUser hits an
#     EPERM the first time".
#   - CAP_NET_ADMIN — netlink AddrAdd to host each service's cluster
#     VIP as a /32 on loopback (internal/agent/dataplane). Without it
#     the dataplane cannot bind VIP listeners and service-to-service
#     traffic (e.g. gateway → mongo.shared.rune) has no delivery path.
# Installed via the systemd unit (not file caps) so the caps travel
# with the unit and survive upgrade-server.sh binary swaps.
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER CAP_NET_ADMIN

[Install]
WantedBy=multi-user.target
`

// Render writes the unit text to w using opts. Returns the byte count
// written and any I/O error.
func Render(w io.Writer, opts UnitOptions) (int64, error) {
	if err := opts.Validate(); err != nil {
		return 0, err
	}
	t, err := template.New("runed.service").Parse(unitTemplate)
	if err != nil {
		// Unreachable in practice — unitTemplate is a constant — but a
		// programming error here is easier to diagnose with a real error
		// than a panic on a happy-path call site.
		return 0, fmt.Errorf("systemd: parse unit template: %w", err)
	}
	cw := &countingWriter{w: w}
	if err := t.Execute(cw, opts); err != nil {
		return cw.n, fmt.Errorf("systemd: execute unit template: %w", err)
	}
	return cw.n, nil
}

// RenderString is a convenience for callers that want the rendered
// unit as a string (tests, the `runed print-unit` subcommand). The
// returned string is empty if opts validation fails — callers that
// care should check the error.
func RenderString(opts UnitOptions) (string, error) {
	var b strings.Builder
	if _, err := Render(&b, opts); err != nil {
		return "", err
	}
	return b.String(), nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
