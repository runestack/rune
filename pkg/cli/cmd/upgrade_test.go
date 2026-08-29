package cmd

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestWatchTimeoutDiagnosis(t *testing.T) {
	from, target := "v0.0.1-dev.147", "v0.0.1-dev.150"
	cases := []struct {
		name          string
		lastVersion   string
		lastReady     bool
		terminalEvent string
		wantSubstring string
	}{
		{"crash-looping", target, false, "", "journalctl -u runed "},
		{"rolled back", from, false, "UpgradeRolledBack: rolled-back v0.0.1-dev.147", "rolled back"},
		// The stager's UpgradeStaged is emitted on EVERY attempt before
		// the trigger exists, so only terminal events count — an empty
		// terminalEvent must diagnose "never applied", not fall through.
		{"no outcome recorded", from, false, "", "journalctl -u runed-upgrade"},
		{"failed closed", from, false, "UpgradeFailed: sha mismatch", "journalctl -u runed-upgrade"},
		{"nobody answering", "", false, "", "mid-rollback"},
	}
	for _, tc := range cases {
		err := watchTimeoutDiagnosis("prod", from, target, tc.lastVersion, tc.lastReady, tc.terminalEvent)
		if err == nil || !strings.Contains(err.Error(), tc.wantSubstring) {
			t.Fatalf("%s: got %v, want substring %q", tc.name, err, tc.wantSubstring)
		}
	}
}

func TestClassifyStageError(t *testing.T) {
	target := "v0.0.1-dev.150"
	both := &upgradeOptions{}
	serverOnly := &upgradeOptions{serverOnly: true}

	// Non-admin degrades to client-only (proceed=false, no hard error) —
	// unless the server half was the entire request.
	if p, _, _, hard := classifyStageError(status.Error(codes.PermissionDenied, "denied"), target, both); p || hard != nil {
		t.Fatalf("PermissionDenied: proceed=%v hard=%v", p, hard)
	}
	if _, _, _, hard := classifyStageError(status.Error(codes.PermissionDenied, "denied"), target, serverOnly); hard == nil {
		t.Fatal("PermissionDenied with --server must be a hard error")
	}

	// A dropped connection means possibly staged: watch, don't fail.
	p, watch, _, hard := classifyStageError(status.Error(codes.Unavailable, "gone"), target, both)
	if !p || !watch || hard != nil {
		t.Fatalf("Unavailable: proceed=%v watch=%v hard=%v", p, watch, hard)
	}

	// Pre-RUNE-321 server degrades with the one-time SSH remedy.
	if p, _, _, hard := classifyStageError(status.Error(codes.Unimplemented, "old"), target, both); p || hard != nil {
		t.Fatalf("Unimplemented: proceed=%v hard=%v", p, hard)
	}
	if _, _, _, hard := classifyStageError(status.Error(codes.Unimplemented, "old"), target, serverOnly); hard == nil || !strings.Contains(hard.Error(), "upgrade-server.sh") {
		t.Fatalf("Unimplemented with --server must hand over the one-liner: %v", hard)
	}

	// Precondition slugs pick distinct paths.
	if _, _, _, hard := classifyStageError(status.Error(codes.FailedPrecondition, "reason=upgrade-in-progress: v is staging"), target, both); hard == nil || !strings.Contains(hard.Error(), "already in progress") {
		t.Fatalf("in-progress: %v", hard)
	}
	if p, _, _, hard := classifyStageError(status.Error(codes.FailedPrecondition, "reason=units-missing: not installed"), target, both); p || hard != nil {
		t.Fatalf("units-missing must degrade: proceed=%v hard=%v", p, hard)
	}
}

// Every way classifyStageError can skip the server half, and whether each
// is something a human can go and fix. (The non-linux server degrades the
// same way, from upgradeServer rather than here.) A capability the caller never had is
// not a failure of the command, so it must not exit non-zero — an exit
// code that always fires is one people learn to ignore.
func TestClassifyStageError_ActionableSplit(t *testing.T) {
	both := &upgradeOptions{}
	cases := []struct {
		name       string
		err        error
		actionable bool
	}{
		{"no admin token", status.Error(codes.PermissionDenied, "denied"), false},
		{"not systemd", status.Error(codes.FailedPrecondition, "reason=no-systemd: dev"), false},
		{"expired token", status.Error(codes.Unauthenticated, "expired"), true},
		{"server too old", status.Error(codes.Unimplemented, "old"), true},
		{"units missing", status.Error(codes.FailedPrecondition, "reason=units-missing: not installed"), true},
	}
	for _, tc := range cases {
		proceed, _, skipped, hard := classifyStageError(tc.err, "v0.0.1-dev.150", both)
		if proceed || hard != nil {
			t.Fatalf("%s: expected a degrade, got proceed=%v hard=%v", tc.name, proceed, hard)
		}
		if skipped == nil {
			t.Fatalf("%s: degrade must report why it skipped", tc.name)
		}
		if skipped.actionable != tc.actionable {
			t.Fatalf("%s: actionable=%v, want %v", tc.name, skipped.actionable, tc.actionable)
		}
		if skipped.reason == "" {
			t.Fatalf("%s: reason must not be empty", tc.name)
		}
		if tc.actionable && skipped.nextStep == "" {
			t.Fatalf("%s: an actionable degrade must say what to do", tc.name)
		}
	}
}

// The diagnosis must not claim more than the evidence supports: an absent
// outcome means it was not recorded, which is not the same as an upgrade
// that never ran, and the journal is the record that settles it.
func TestWatchTimeoutDiagnosis_NamesTheHostAndDoesNotOverclaim(t *testing.T) {
	err := watchTimeoutDiagnosis("prod", "v0.0.1-dev.147", "v0.0.1-dev.150", "v0.0.1-dev.147", false, "")
	if err == nil {
		t.Fatal("expected a diagnosis")
	}
	msg := err.Error()
	if !strings.Contains(msg, "prod") {
		t.Fatalf("the diagnosis must name the host: %q", msg)
	}
	if strings.Contains(msg, "no upgrade was applied") {
		t.Fatalf("must not assert the apply never happened: %q", msg)
	}
	if !strings.Contains(msg, "journalctl -u runed-upgrade") {
		t.Fatalf("must point at the durable record: %q", msg)
	}
}
