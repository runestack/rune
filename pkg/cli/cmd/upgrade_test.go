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
		{"never applied", from, false, "", "runed-upgrade.path"},
		{"failed closed", from, false, "UpgradeFailed: sha mismatch", "journalctl -u runed-upgrade"},
		{"nobody answering", "", false, "", "mid-rollback"},
	}
	for _, tc := range cases {
		err := watchTimeoutDiagnosis(from, target, tc.lastVersion, tc.lastReady, tc.terminalEvent)
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
	if p, _, hard := classifyStageError(status.Error(codes.PermissionDenied, "denied"), target, both); p || hard != nil {
		t.Fatalf("PermissionDenied: proceed=%v hard=%v", p, hard)
	}
	if _, _, hard := classifyStageError(status.Error(codes.PermissionDenied, "denied"), target, serverOnly); hard == nil {
		t.Fatal("PermissionDenied with --server must be a hard error")
	}

	// A dropped connection means possibly staged: watch, don't fail.
	p, watch, hard := classifyStageError(status.Error(codes.Unavailable, "gone"), target, both)
	if !p || !watch || hard != nil {
		t.Fatalf("Unavailable: proceed=%v watch=%v hard=%v", p, watch, hard)
	}

	// Pre-RUNE-321 server degrades with the one-time SSH remedy.
	if p, _, hard := classifyStageError(status.Error(codes.Unimplemented, "old"), target, both); p || hard != nil {
		t.Fatalf("Unimplemented: proceed=%v hard=%v", p, hard)
	}
	if _, _, hard := classifyStageError(status.Error(codes.Unimplemented, "old"), target, serverOnly); hard == nil || !strings.Contains(hard.Error(), "upgrade-server.sh") {
		t.Fatalf("Unimplemented with --server must hand over the one-liner: %v", hard)
	}

	// Precondition slugs pick distinct paths.
	if _, _, hard := classifyStageError(status.Error(codes.FailedPrecondition, "reason=upgrade-in-progress: v is staging"), target, both); hard == nil || !strings.Contains(hard.Error(), "already in progress") {
		t.Fatalf("in-progress: %v", hard)
	}
	if p, _, hard := classifyStageError(status.Error(codes.FailedPrecondition, "reason=units-missing: not installed"), target, both); p || hard != nil {
		t.Fatalf("units-missing must degrade: proceed=%v hard=%v", p, hard)
	}
}
