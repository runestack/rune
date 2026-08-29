//go:build linux

package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckFloor(t *testing.T) {
	dir := t.TempDir()
	floorPath := filepath.Join(dir, "version-floor")
	a := &Applier{Runtime: ApplierRuntime{CurrentVersion: "v0.0.1-dev.150", FloorPath: floorPath}}

	// Downgrade without the opt-in is refused even with no floor.
	err := a.checkFloor(&Manifest{Version: "v0.0.1-dev.140"})
	if err == nil || !strings.Contains(err.Error(), "did not opt into a downgrade") {
		t.Fatalf("downgrade without opt-in: %v", err)
	}

	// Absent floor + upgrade: allowed.
	if err := a.checkFloor(&Manifest{Version: "v0.0.1-dev.151"}); err != nil {
		t.Fatalf("absent floor upgrade: %v", err)
	}

	// Below-floor is refused even with the opt-in; the error carries the
	// exact root remedy.
	if err := WriteFloor(floorPath, "v0.0.1-dev.145"); err != nil {
		t.Fatal(err)
	}
	err = a.checkFloor(&Manifest{Version: "v0.0.1-dev.140", AllowDowngrade: true})
	if err == nil || !strings.Contains(err.Error(), floorPath) {
		t.Fatalf("below floor: %v", err)
	}

	// At-or-above floor downgrade with opt-in: allowed.
	if err := a.checkFloor(&Manifest{Version: "v0.0.1-dev.146", AllowDowngrade: true}); err != nil {
		t.Fatalf("above-floor downgrade with opt-in: %v", err)
	}

	// Corrupt floor fails closed.
	if err := os.WriteFile(floorPath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.checkFloor(&Manifest{Version: "v0.0.1-dev.151"}); err == nil {
		t.Fatal("unparseable floor must refuse")
	}
}

func TestTargetRequiresReady(t *testing.T) {
	// Upgrades demand the ready flag; downgrades may target pre-ready
	// builds and must not (they would always roll back).
	if !targetRequiresReady("v0.0.1-dev.151", "v0.0.1-dev.150") {
		t.Fatal("upgrade must require ready")
	}
	if targetRequiresReady("v0.0.1-dev.140", "v0.0.1-dev.150") {
		t.Fatal("downgrade must not require ready")
	}
	if !targetRequiresReady("v0.0.1-dev.151", "dev") {
		t.Fatal("unparseable current must default to requiring ready")
	}
}

// TestConsume_ErrorStillConsumesReady pins the no-refire contract: a
// consume that fails (here: ready exists but the manifest is missing)
// must still remove `ready`, or systemd refires the oneshot in a loop
// until the path unit trips into failed state and the feature is wedged.
func TestConsume_ErrorStillConsumesReady(t *testing.T) {
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, readyName), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Applier{StagingDir: staging, Runtime: ApplierRuntime{CurrentVersion: "v0.0.1-dev.150", Now: time.Now}}
	if _, _, err := a.consume(); err == nil {
		t.Fatal("consume with no manifest must error")
	}
	if _, err := os.Stat(filepath.Join(staging, readyName)); !os.IsNotExist(err) {
		t.Fatal("a failed consume must still remove the ready trigger")
	}
}

// The Terraform modules provision their own runed.service: EnvironmentFile=
// carrying registry credentials, and no User= because runed runs as root
// there. Rendering the canonical template over it drops both — and the
// upgrade would still report success, which is the worst outcome the
// applier can produce.
func TestUnitRefreshUnsafe(t *testing.T) {
	const terraform = `[Unit]
Description=Rune Server

[Service]
Type=simple
EnvironmentFile=-/etc/rune/runed.env
ExecStart=/usr/local/bin/runed
Restart=on-failure

[Install]
WantedBy=multi-user.target
`
	const rendered = `[Unit]
Description=Rune Server

[Service]
Type=simple
User=rune
Group=rune
ExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml
Restart=on-failure
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
`
	if why := unitRefreshUnsafe(terraform, rendered); why == "" {
		t.Fatal("a unit carrying EnvironmentFile= must not be refreshed")
	} else if !strings.Contains(why, "EnvironmentFile") {
		t.Fatalf("the reason must name the directive at risk, got %q", why)
	}

	// Same unit minus the foreign directive still runs as root, so the
	// refresh would silently demote the service user.
	rootOnly := strings.Replace(terraform, "EnvironmentFile=-/etc/rune/runed.env\n", "", 1)
	if why := unitRefreshUnsafe(rootOnly, rendered); why == "" {
		t.Fatal("a root-running unit must not be silently given User=")
	}

	// A unit this template authored refreshes freely — that is the drift
	// fix the refresh exists for.
	ours := strings.Replace(rendered, "AmbientCapabilities=CAP_NET_BIND_SERVICE\n", "", 1)
	if why := unitRefreshUnsafe(ours, rendered); why != "" {
		t.Fatalf("adding a directive must stay safe, got %q", why)
	}
	if why := unitRefreshUnsafe("", rendered); why != "" {
		t.Fatalf("absent unit must be safe to write, got %q", why)
	}
}

// The staged version is interpolated into the release URL this applier
// fetches its checksums from — the anchor for every byte it installs. A
// version that is not a semver tag could point that fetch somewhere else,
// so parseManifest refuses it before it can reach DownloadURL. checkFloor
// cannot be relied on for this: it returns early on a floor-less host.
func TestParseManifest_RejectsNonSemverVersion(t *testing.T) {
	for _, bad := range []string{
		`{"version":"../../../attacker/evil/releases/download/v1"}`,
		`{"version":"v1 v2"}`,
		`{"version":"not-a-version"}`,
		`{"version":""}`,
	} {
		if _, err := parseManifest([]byte(bad)); err == nil {
			t.Fatalf("manifest %s must be refused", bad)
		}
	}
	m, err := parseManifest([]byte(`{"version":"v0.0.1-dev.150"}`))
	if err != nil || m.Version != "v0.0.1-dev.150" {
		t.Fatalf("a real tag must parse: %v %v", m, err)
	}
}

// A dropped ExecStart flag is the worst shape of all: the server restarts
// against the default data dir, opens an empty store, passes verification
// and reports success with every service gone.
func TestUnitRefreshUnsafe_ExecStartFlags(t *testing.T) {
	render := "[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml\n"
	for _, live := range []string{
		"[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --data-dir /mnt/data\n",
		"[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml --http-addr :9000\n",
		"[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --dev-mode\n",
		// The equals form is the same flag and must be caught too.
		"[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --data-dir=/mnt/data\n",
	} {
		if why := unitRefreshUnsafe(live, render); why == "" {
			t.Fatalf("a flag the render cannot reproduce must block the refresh:\n%s", live)
		}
	}
	// The flags the render does reproduce are not a reason to refuse —
	// including in the equals form, which is the same flag written
	// differently and must not read as one the template cannot express.
	for _, same := range []string{
		"[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml\n",
		"[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --config=/etc/rune/runefile.toml\n",
	} {
		if why := unitRefreshUnsafe(same, render); why != "" {
			t.Fatalf("a flag the render does reproduce must stay safe, got %q\n%s", why, same)
		}
	}
}

func TestUnitRefreshUnsafe_FalsePositives(t *testing.T) {
	render := "[Unit]\nDescription=Rune Server\n\n[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml\n"

	// Documentation= is shipped in examples/config/runed.service and changes
	// nothing about how the service runs; refusing on it would disable the
	// refresh for ever over a cosmetic line.
	doc := "[Unit]\nDescription=Rune Server\nDocumentation=https://runestack.io\n\n[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml\n"
	if why := unitRefreshUnsafe(doc, render); why != "" {
		t.Fatalf("Documentation= must not block the refresh, got %q", why)
	}

	// A wrapped ExecStart is one directive, not two.
	wrapped := "[Unit]\nDescription=Rune Server\n\n[Service]\nUser=rune\nGroup=rune\nExecStart=/usr/local/bin/runed \\\n  --config=/etc/rune/runefile.toml\n"
	if why := unitRefreshUnsafe(wrapped, render); strings.Contains(why, "--config") {
		t.Fatalf("a continuation line must not register as a directive: %q", why)
	}
}

// setcap replaces the capability set rather than adding to it, so keeping
// only one clause of a multi-clause set silently drops the rest — on a host
// that had more than this project usually grants.
func TestParseGetcap(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/runed cap_net_bind_service=ep":                 "cap_net_bind_service=ep",
		"/usr/local/bin/runed cap_net_bind_service=ep cap_sys_admin=p": "cap_net_bind_service=ep cap_sys_admin=p",
		"/usr/local/bin/runed = cap_net_bind_service+ep":               "cap_net_bind_service+ep",
		"/usr/local/bin/runed":                                         "",
		"":                                                             "",
		"/usr/local/bin/runed something-else":                          "",
	}
	for out, want := range cases {
		if got := parseGetcap(out); got != want {
			t.Fatalf("parseGetcap(%q) = %q, want %q", out, got, want)
		}
	}
}
