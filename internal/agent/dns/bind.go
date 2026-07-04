package dns

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/runestack/rune/pkg/log"
)

// DevDefaultBindAddr is the loopback fallback for laptop dev (macOS cannot
// bind 127.0.0.123 without an interface alias, and port 53 requires root).
// Host-side .rune access in dev mode goes through the dataplane on 127.0.0.1;
// this address keeps the embedded resolver available for dig/host testing.
const DevDefaultBindAddr = "127.0.0.1:15353"

// unprivilegedPortStart mirrors the Linux default net.ipv4.ip_unprivileged_port_start.
// Ports below it require CAP_NET_BIND_SERVICE (or root). Used only to classify
// bind failures for diagnostics — the actual bind still comes from probeBind.
const unprivilegedPortStart = 1024

// BindFailure records an address that could not host a DNS listener and why.
// Returned by FilterBindable so callers can build an actionable diagnostic
// (see DiagnoseEmptyBind) instead of a bare "no bindable addresses".
type BindFailure struct {
	Addr string
	Err  error
}

// isPrivilegedPermission reports whether this failure is a permission error
// (EACCES/EPERM) on a privileged port — the fingerprint of a runed running
// without CAP_NET_BIND_SERVICE.
func (f BindFailure) isPrivilegedPermission() bool {
	if !errors.Is(f.Err, os.ErrPermission) {
		return false
	}
	_, portStr, err := net.SplitHostPort(f.Addr)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return false
	}
	return port > 0 && port < unprivilegedPortStart
}

// FilterBindable returns the subset of addrs that can host both a UDP and TCP
// listener, in input order with duplicates removed. Addresses that fail the
// probe are logged at Warn with the underlying error and returned in skipped so
// callers can explain an empty result — Docker Desktop on macOS reports bridge
// gateways such as 172.17.0.1 that are not host-bindable, and a runed lacking
// CAP_NET_BIND_SERVICE fails every privileged :53 bind.
func FilterBindable(addrs []string, logger log.Logger) (bindable []string, skipped []BindFailure) {
	seen := make(map[string]struct{}, len(addrs))
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		if err := probeBind(addr); err != nil {
			skipped = append(skipped, BindFailure{Addr: addr, Err: err})
			if logger != nil {
				logger.Warn("dns: address not bindable; skipping",
					log.Str("addr", addr), log.Err(err))
			}
			continue
		}
		bindable = append(bindable, addr)
	}
	return bindable, skipped
}

// DiagnoseEmptyBind returns an operator-facing explanation for why the bindable
// set came back empty, given the skipped candidates. When every candidate
// failed a privileged port with a permission error it names the likely cause —
// a missing CAP_NET_BIND_SERVICE, typically a hand-authored systemd unit — and
// the fix. Otherwise it lists what was tried.
func DiagnoseEmptyBind(skipped []BindFailure) string {
	if len(skipped) == 0 {
		return "no DNS bind candidates were produced (no docker bridge gateways discovered and no loopback default?)"
	}
	parts := make([]string, 0, len(skipped))
	allPrivPerm := true
	for _, f := range skipped {
		parts = append(parts, fmt.Sprintf("%s (%v)", f.Addr, f.Err))
		if !f.isPrivilegedPermission() {
			allPrivPerm = false
		}
	}
	tried := strings.Join(parts, ", ")
	if allPrivPerm {
		return "every candidate failed binding a privileged port (<1024) with a permission error, " +
			"so runed lacks CAP_NET_BIND_SERVICE. If you hand-authored " +
			"/etc/systemd/system/runed.service, regenerate it with `runed print-systemd > " +
			"/etc/systemd/system/runed.service`, then `systemctl daemon-reload && systemctl restart runed` " +
			"(the canonical unit sets AmbientCapabilities=CAP_NET_BIND_SERVICE). Tried: " + tried
	}
	return "tried: " + tried
}

// probeBind returns nil when both a UDP and TCP listener can be opened on addr,
// else the first bind error (its errno is what DiagnoseEmptyBind classifies).
func probeBind(addr string) error {
	uc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	_ = uc.Close()
	tc, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	_ = tc.Close()
	return nil
}
