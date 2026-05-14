package systemd

import (
	"strings"
	"testing"
)

func TestRenderString_Defaults(t *testing.T) {
	out, err := RenderString(DefaultUnitOptions())
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}

	// Each assertion is a contract the on-disk unit must satisfy. If any
	// of these breaks, every host that runs `upgrade-server.sh --refresh-unit`
	// inherits the regression — keep the expectations explicit.
	wantContains := []string{
		"[Unit]",
		"Description=Rune Server",
		"After=network-online.target docker.service",
		"[Service]",
		"Type=simple",
		"User=rune",
		"Group=rune",
		"ExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml",
		"Restart=on-failure",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER",
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER",
		"SupplementaryGroups=docker",
		"[Install]",
		"WantedBy=multi-user.target",
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("rendered unit missing required directive %q\n--- rendered unit ---\n%s", w, out)
		}
	}
}

// Customizing the User/Group/BinaryPath fields must propagate to every
// place they're referenced in the unit. Locks in the substitution
// surface so a future refactor can't accidentally hardcode one of them.
func TestRenderString_AllSubstitutionsApplied(t *testing.T) {
	opts := UnitOptions{
		User:       "app",
		Group:      "ops",
		BinaryPath: "/opt/rune/bin/runed",
		ConfigPath: "/etc/runed.toml",
	}
	out, err := RenderString(opts)
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}

	cases := map[string]string{
		"custom user":        "User=app",
		"custom group":       "Group=ops",
		"custom binary path": "ExecStart=/opt/rune/bin/runed --config /etc/runed.toml",
	}
	for label, want := range cases {
		if !strings.Contains(out, want) {
			t.Errorf("%s: unit missing %q\n--- rendered unit ---\n%s", label, want, out)
		}
	}

	// And nothing from the defaults must leak when fully overridden.
	for _, leak := range []string{"User=rune", "Group=rune", "/usr/local/bin/runed"} {
		if strings.Contains(out, leak) {
			t.Errorf("default %q leaked into unit despite override: %s", leak, out)
		}
	}
}

// An empty ConfigPath must omit --config so runed falls back to its
// auto-discovery search order. Pre-existing operators rely on this.
func TestRenderString_EmptyConfigPathOmitsFlag(t *testing.T) {
	opts := DefaultUnitOptions()
	opts.ConfigPath = ""
	out, err := RenderString(opts)
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}

	if !strings.Contains(out, "ExecStart=/usr/local/bin/runed\n") {
		t.Errorf("empty ConfigPath should produce bare ExecStart line; got:\n%s", out)
	}
	// Targeting the ExecStart line itself; the comment block above it
	// mentions --config in prose and would false-positive a bare Contains.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ExecStart=") && strings.Contains(line, "--config") {
			t.Errorf("ExecStart must not carry --config when ConfigPath is empty; got: %s", line)
		}
	}
}

func TestRenderString_ValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		opts UnitOptions
		want string
	}{
		{
			name: "missing user",
			opts: UnitOptions{Group: "rune", BinaryPath: "/usr/local/bin/runed"},
			want: "User is required",
		},
		{
			name: "missing group",
			opts: UnitOptions{User: "rune", BinaryPath: "/usr/local/bin/runed"},
			want: "Group is required",
		},
		{
			name: "missing binary path",
			opts: UnitOptions{User: "rune", Group: "rune"},
			want: "BinaryPath is required",
		},
		{
			name: "whitespace-only user",
			opts: UnitOptions{User: "   ", Group: "rune", BinaryPath: "/usr/local/bin/runed"},
			want: "User is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RenderString(tc.opts)
			if err == nil {
				t.Fatalf("expected error, got nil. Output was: %q", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error to contain %q, got: %v", tc.want, err)
			}
			if out != "" {
				t.Errorf("RenderString should return empty on validation failure; got %q", out)
			}
		})
	}
}

// Regression for the symptom Propeller hit: pre-v0.0.1-dev.45 the
// systemd unit on their droplet was missing AmbientCapabilities=
// because the install-server.sh template they were provisioned from
// pre-dated that line. Lock the directive into pkg/systemd so any
// fresh render (and therefore any `upgrade-server.sh --refresh-unit`
// run) includes it.
func TestRenderString_AmbientCapabilitiesAlwaysSet(t *testing.T) {
	out, err := RenderString(DefaultUnitOptions())
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}
	if !strings.Contains(out, "AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER") {
		t.Errorf("AmbientCapabilities must include the full cap set; got:\n%s", out)
	}
	if !strings.Contains(out, "CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER") {
		t.Errorf("CapabilityBoundingSet must include the full cap set; got:\n%s", out)
	}
}
