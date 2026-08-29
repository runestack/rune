package upgrade

import (
	"strings"

	"github.com/runestack/rune/pkg/systemd"
)

// ParseUnitShow extracts User/Group and the runed binary path + --config
// argument from `systemctl show runed -p User -p Group -p ExecStart`
// output. systemctl's canonical output is the source of truth for the live
// unit's values — parsing the unit file would miss drop-ins, and rendering
// print-systemd with defaults would clobber a custom install (the failure
// the old upgrade-server.sh --refresh-unit shipped).
//
// ExecStart's show format is:
//
//	ExecStart={ path=/usr/local/bin/runed ; argv[]=/usr/local/bin/runed --config /etc/rune/runefile.toml ; ignore_errors=no ; ... }
//
// Returns the binary path and config path found ("" when absent); fills
// User/Group on vals when present.
func ParseUnitShow(out string, vals *systemd.UnitOptions) (binPath, configPath string) {
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
			binPath, configPath = parseExecStartShow(strings.TrimPrefix(line, "ExecStart="))
		}
	}
	return binPath, configPath
}

// parseExecStartShow parses the `{ path=... ; argv[]=... ; ... }` body of
// a systemctl-show ExecStart property.
func parseExecStartShow(body string) (binPath, configPath string) {
	for _, part := range strings.Split(strings.Trim(body, "{} "), ";") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "path="); ok {
			binPath = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(part, "argv[]="); ok {
			args := strings.Fields(v)
			for i, a := range args {
				if a == "--config" && i+1 < len(args) {
					configPath = args[i+1]
				}
			}
		}
	}
	return binPath, configPath
}
