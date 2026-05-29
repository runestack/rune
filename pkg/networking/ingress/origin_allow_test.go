package ingress

import (
	"net"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func TestRoute_PeerAllowed(t *testing.T) {
	cf := []*net.IPNet{mustCIDR(t, "173.245.48.0/20"), mustCIDR(t, "103.21.244.0/22")}

	// Empty allowlist => allow all (never deny-all).
	open := Route{}
	if !open.PeerAllowed(net.ParseIP("8.8.8.8")) {
		t.Error("empty allowlist must allow all")
	}
	if !open.PeerAllowed(nil) {
		t.Error("empty allowlist must allow even a nil peer")
	}

	r := Route{AllowCIDRs: cf}
	if !r.PeerAllowed(net.ParseIP("173.245.48.5")) {
		t.Error("in-range peer should be allowed")
	}
	if r.PeerAllowed(net.ParseIP("8.8.8.8")) {
		t.Error("out-of-range peer should be denied")
	}
	// Fail closed: unknown peer under an active allowlist is denied.
	if r.PeerAllowed(nil) {
		t.Error("nil peer must be denied when an allowlist is set")
	}
}

func TestPeerIP(t *testing.T) {
	cases := map[string]string{
		"173.245.48.5:54321": "173.245.48.5",
		"173.245.48.5":       "173.245.48.5", // bare IP, no port
		"[2606:4700::1]:443": "2606:4700::1",
		"garbage":            "", // unparseable => nil
	}
	for in, want := range cases {
		got := peerIP(in)
		if want == "" {
			if got != nil {
				t.Errorf("peerIP(%q) = %v, want nil", in, got)
			}
			continue
		}
		if got == nil || !got.Equal(net.ParseIP(want)) {
			t.Errorf("peerIP(%q) = %v, want %s", in, got, want)
		}
	}
}

func TestMTLSGate(t *testing.T) {
	noMTLS := Route{}
	mtls := Route{RequireClientCert: true}

	cases := []struct {
		name     string
		rt       Route
		scheme   string
		hasPool  bool
		wantOK   bool
		wantCode int
	}{
		// Routes without clientCert always proceed.
		{"no-mtls http", noMTLS, "http", false, true, 0},
		{"no-mtls https", noMTLS, "https", false, true, 0},
		// mTLS-required: plaintext is never allowed (no client cert possible).
		{"mtls http refused", mtls, "http", true, false, 403},
		// mTLS-required on https but CA pool not loaded → fail closed.
		{"mtls https no pool", mtls, "https", false, false, 503},
		// mTLS-required, https, pool loaded → proceed (handshake already
		// verified the cert).
		{"mtls https with pool", mtls, "https", true, true, 0},
	}
	for _, c := range cases {
		code, ok := mtlsGate(c.rt, c.scheme, c.hasPool)
		if ok != c.wantOK || code != c.wantCode {
			t.Errorf("%s: mtlsGate = (%d, %v), want (%d, %v)", c.name, code, ok, c.wantCode, c.wantOK)
		}
	}
}
