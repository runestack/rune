package systemd

import (
	"strings"
	"testing"
)

func TestRenderUpgradeUnits(t *testing.T) {
	opts := UpgradeUnitOptions{
		StagingDir: "/var/lib/rune/upgrade",
		BinaryPath: "/usr/local/bin/runed",
		ConfigPath: "/etc/rune/runefile.toml",
	}
	svc, err := RenderUpgradeService(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Type=oneshot",
		"ConditionPathExists=/var/lib/rune/upgrade/ready",
		"ExecStart=/usr/local/bin/runed apply-upgrade --staging /var/lib/rune/upgrade --config /etc/rune/runefile.toml",
	} {
		if !strings.Contains(svc, want) {
			t.Fatalf("service unit missing %q:\n%s", want, svc)
		}
	}
	if strings.Contains(svc, "[Install]") {
		t.Fatal("the oneshot must have no [Install] — only the path unit starts it")
	}

	path, err := RenderUpgradePath(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PathExists=/var/lib/rune/upgrade/ready",
		"Unit=runed-upgrade.service",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(path, want) {
			t.Fatalf("path unit missing %q:\n%s", want, path)
		}
	}

	// Empty config omits the flag instead of rendering `--config `.
	opts.ConfigPath = ""
	svc, _ = RenderUpgradeService(opts)
	if strings.Contains(svc, "--config") {
		t.Fatal("empty ConfigPath must omit --config")
	}

	if _, err := RenderUpgradeService(UpgradeUnitOptions{BinaryPath: "/x"}); err == nil {
		t.Fatal("missing StagingDir must error")
	}
}
