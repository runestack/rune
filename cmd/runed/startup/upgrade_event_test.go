package startup

import (
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/upgrade"
)

// The CLI matches the declined-refresh case on its own slug rather than on
// the wording of the message, so the mapping is the contract between them.
func TestUpgradeEvent(t *testing.T) {
	cases := []struct {
		name       string
		res        upgrade.Result
		wantReason string
		wantLevel  types.EventLevel
	}{
		{"plain success", upgrade.Result{Outcome: "success"}, "UpgradeApplied", types.EventLevelInfo},
		{"success with a declined unit refresh",
			upgrade.Result{Outcome: "success", Reason: "left /etc/systemd/system/runed.service unchanged: it sets EnvironmentFile"},
			"UpgradeAppliedUnitUnchanged", types.EventLevelInfo},
		{"rolled back", upgrade.Result{Outcome: "rolled-back", Reason: "verify failed"}, "UpgradeRolledBack", types.EventLevelWarn},
		{"failed", upgrade.Result{Outcome: "failed", Reason: "digest mismatch"}, "UpgradeFailed", types.EventLevelWarn},
		{"noop", upgrade.Result{Outcome: "noop", Reason: "already at target"}, "UpgradeSkipped", types.EventLevelInfo},
	}
	for _, tc := range cases {
		level, reason, msg := upgradeEvent(&tc.res)
		if reason != tc.wantReason {
			t.Fatalf("%s: reason = %q, want %q", tc.name, reason, tc.wantReason)
		}
		if level != tc.wantLevel {
			t.Fatalf("%s: level = %q, want %q", tc.name, level, tc.wantLevel)
		}
		if tc.res.Reason != "" && !strings.Contains(msg, tc.res.Reason) {
			t.Fatalf("%s: message must carry the reason: %q", tc.name, msg)
		}
	}
}
