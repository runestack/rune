package ingressctl

import "testing"

func TestParseAllowCIDRs(t *testing.T) {
	// nil/empty => nil (route's empty allowlist = allow all).
	if got := parseAllowCIDRs(nil, "h", nil); got != nil {
		t.Errorf("nil input => %v, want nil", got)
	}

	// Valid entries parse through.
	got := parseAllowCIDRs([]string{"10.0.0.0/8", "192.168.0.0/16"}, "h", nil)
	if len(got) != 2 {
		t.Fatalf("got %d nets, want 2", len(got))
	}

	// Invalid entries are skipped (tightening), not dropped wholesale: a
	// good entry alongside a bad one still yields the good one.
	got = parseAllowCIDRs([]string{"bad", "10.0.0.0/8"}, "h", nil)
	if len(got) != 1 {
		t.Fatalf("got %d nets, want 1 (bad entry skipped)", len(got))
	}
}
