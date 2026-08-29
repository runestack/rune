package upgrade

import (
	"strings"

	"github.com/runestack/rune/pkg/systemd"
)

// ParseUnitShow extracts User/Group and the runed binary path + --config
// argument from `systemctl show runed -p User -p Group -p ExecStart`
// output. systemctl's canonical output is the source of truth for the live
// unit's values — parsing the unit file would miss drop-ins, and rendering
// print-systemd with defaults would clobber a custom install, which is what
// scripts/upgrade-server.sh --refresh-unit still does.
//
// ExecStart's show format is:
//
//	ExecStart={ path=/usr/local/bin/runed ; argv[]=/usr/local/bin/runed --config /etc/rune/runefile.toml ; ignore_errors=no ; ... }
//
// Returns the binary path, the --config path, and runed's bind address when
// the unit pins one by flag or environment ("" when absent); fills
// User/Group on vals when present.
func ParseUnitShow(out string, vals *systemd.UnitOptions) (binPath, configPath, grpcAddr string) {
	// Collected separately so precedence does not depend on the order
	// systemctl happens to print the properties in.
	var flagAddr, envAddr string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "User="):
			if v := strings.TrimPrefix(line, "User="); v != "" {
				vals.User = v
			}
		case strings.HasPrefix(line, "Group="):
			if v := strings.TrimPrefix(line, "Group="); v != "" {
				vals.Group = v
			}
		case strings.HasPrefix(line, "ExecStart="):
			binPath, configPath, flagAddr = parseExecStartShow(strings.TrimPrefix(line, "ExecStart="))
		case strings.HasPrefix(line, "Environment="):
			// runed resolves its bind address flag > env > runefile, so an
			// Environment= (typically from a drop-in) outranks the file.
			for _, kv := range strings.Fields(strings.TrimPrefix(line, "Environment=")) {
				if v, ok := strings.CutPrefix(kv, "RUNE_SERVER_GRPC_ADDRESS="); ok {
					envAddr = v
				}
			}
		}
	}
	grpcAddr = flagAddr
	if grpcAddr == "" {
		grpcAddr = envAddr
	}
	return binPath, configPath, grpcAddr
}

// parseExecStartShow parses the `{ path=... ; argv[]=... ; ... }` body of
// a systemctl-show ExecStart property.
func parseExecStartShow(body string) (binPath, configPath, grpcAddr string) {
	for _, part := range strings.Split(strings.Trim(body, "{} "), ";") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "path="); ok {
			binPath = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(part, "argv[]="); ok {
			args := strings.Fields(v)
			if v, ok := execStartFlag(args, "--config"); ok {
				configPath = v
			}
			if v, ok := execStartFlag(args, "--grpc-addr"); ok {
				grpcAddr = v
			}
		}
	}
	return binPath, configPath, grpcAddr
}

// execStartFlag finds want in an ExecStart argv, accepting both
// "--flag value" and "--flag=value", and a single leading dash for either
// (Go's flag package does). unitRefreshUnsafe normalises the same way: if
// the two disagree, the guard certifies a refresh whose render then drops
// the value.
func execStartFlag(args []string, want string) (string, bool) {
	for i, a := range args {
		name, inline, hasInline := strings.Cut(a, "=")
		if "--"+strings.TrimLeft(name, "-") != want {
			continue
		}
		if hasInline {
			return inline, true
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
