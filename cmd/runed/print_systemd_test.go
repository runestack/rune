package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPrintSystemd_DefaultsEmitValidUnit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runPrintSystemd(nil, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", rc, stderr.String())
	}

	out := stdout.String()
	for _, w := range []string{
		"[Unit]",
		"[Service]",
		"User=rune",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER",
		"ExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q. Full output:\n%s", w, out)
		}
	}
}

func TestRunPrintSystemd_FlagsPropagate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runPrintSystemd(
		[]string{"--user", "app", "--group", "ops", "--binary", "/opt/rune/runed", "--config", "/etc/runed.toml"},
		&stdout, &stderr,
	)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", rc, stderr.String())
	}

	out := stdout.String()
	for _, w := range []string{
		"User=app",
		"Group=ops",
		"ExecStart=/opt/rune/runed --config /etc/runed.toml",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q. Full output:\n%s", w, out)
		}
	}
}

func TestRunPrintSystemd_EmptyConfigOmitsFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runPrintSystemd([]string{"--config", ""}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ExecStart=/usr/local/bin/runed\n") {
		t.Errorf("expected bare ExecStart with no --config; got:\n%s", stdout.String())
	}
	// Look at the ExecStart line specifically — the comment block above it
	// mentions `--config` in prose, which would false-positive a bare
	// `Contains(out, "--config")` check.
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "ExecStart=") && strings.Contains(line, "--config") {
			t.Errorf("ExecStart line must not include --config when ConfigPath is empty; got: %s", line)
		}
	}
}

func TestRunPrintSystemd_RejectsPositionalArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runPrintSystemd([]string{"extra", "stuff"}, &stdout, &stderr)
	if rc != 2 {
		t.Fatalf("expected exit 2 for positional args, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "unexpected positional argument") {
		t.Errorf("expected error message about positional args; got: %s", stderr.String())
	}
}

func TestRunPrintSystemd_RejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runPrintSystemd([]string{"--what"}, &stdout, &stderr)
	if rc != 2 {
		t.Fatalf("expected exit 2 for unknown flag, got %d", rc)
	}
}

// Empty --user should fail validation in pkg/systemd, propagated as
// exit 1 (render error, not flag-parse error).
func TestRunPrintSystemd_EmptyUserExits1(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runPrintSystemd([]string{"--user", ""}, &stdout, &stderr)
	if rc != 1 {
		t.Fatalf("expected exit 1 for validation failure, got %d. stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "User is required") {
		t.Errorf("expected User-required error; got: %s", stderr.String())
	}
}
