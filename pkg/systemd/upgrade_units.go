package systemd

import (
	"fmt"
	"strings"
	"text/template"
)

// UpgradeUnitOptions parameterizes the RUNE-321 applier units. Like the
// runed unit, these are rendered from the binary (`runed print-systemd
// --upgrade-units` / `--upgrade-path-unit`) so installers and the applier
// itself never carry a second, drift-prone copy — and the applier
// re-renders them from each newly installed binary so they are not frozen
// at bootstrap forever.
type UpgradeUnitOptions struct {
	// StagingDir is <data-dir>/upgrade — where runed stages and where the
	// path unit watches for `ready`.
	StagingDir string

	// BinaryPath is the installed runed the oneshot executes.
	BinaryPath string

	// ConfigPath is the runefile passed via --config; empty omits it.
	ConfigPath string
}

// Validate returns an error if required fields are empty.
func (o UpgradeUnitOptions) Validate() error {
	if strings.TrimSpace(o.StagingDir) == "" {
		return fmt.Errorf("systemd: StagingDir must not be empty")
	}
	if strings.TrimSpace(o.BinaryPath) == "" {
		return fmt.Errorf("systemd: BinaryPath must not be empty")
	}
	return nil
}

// The service is a root oneshot: root is required to swap root-owned
// binaries and restart runed, and keeping the privileged logic in one
// short-lived unit is the point of the stage/apply split. It has no
// [Install] section — only the path unit starts it.
//
// ConditionPathExists keeps the oneshot from running when no upgrade is
// staged: the path unit also activates at boot, and a run with no trigger
// would consume nothing and exit non-zero.
const upgradeServiceTemplate = `[Unit]
Description=Rune server upgrade applier
After=network-online.target
Wants=network-online.target
ConditionPathExists={{.StagingDir}}/ready

[Service]
Type=oneshot
# The applier can legitimately run for minutes when a freshly published
# release makes it ride out CDN 504s. oneshot defaults to no start timeout;
# pinned so a distro changing that default cannot kill an apply mid-swap.
TimeoutStartSec=infinity
ExecStart={{.BinaryPath}} apply-upgrade --staging {{.StagingDir}}{{if .ConfigPath}} --config {{.ConfigPath}}{{end}}
`

// The path unit re-activates the service whenever `ready` exists and the
// service is inactive; the applier consumes `ready` as its first act so a
// finished apply — success or failure — cannot refire.
const upgradePathTemplate = `[Unit]
Description=Watch for a staged Rune server upgrade
After=network-online.target

[Path]
PathExists={{.StagingDir}}/ready
Unit=runed-upgrade.service

[Install]
WantedBy=multi-user.target
`

// RenderUpgradeService renders runed-upgrade.service.
func RenderUpgradeService(opts UpgradeUnitOptions) (string, error) {
	return renderUpgrade("runed-upgrade.service", upgradeServiceTemplate, opts)
}

// RenderUpgradePath renders runed-upgrade.path.
func RenderUpgradePath(opts UpgradeUnitOptions) (string, error) {
	return renderUpgrade("runed-upgrade.path", upgradePathTemplate, opts)
}

func renderUpgrade(name, tmpl string, opts UpgradeUnitOptions) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	t, err := template.New(name).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, opts); err != nil {
		return "", err
	}
	return b.String(), nil
}
