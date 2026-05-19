package main

import (
	"reflect"
	"testing"
)

// TestDnsServerIPs documents the host:port → host transformation used
// to feed docker's --dns flag, which expects bare IPs (or hostnames,
// but Rune always emits IPs). The behaviour matters because a typo
// here means containers silently fall back to upstream DNS and the
// entire <service>.<namespace>.rune service-discovery layer breaks
// with NXDOMAIN — the exact bug this PR fixes.
func TestDnsServerIPs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "default loopback bind",
			in:   []string{"127.0.0.123:53"},
			want: []string{"127.0.0.123"},
		},
		{
			name: "multiple binds preserve order and dedupe by host",
			in:   []string{"127.0.0.123:53", "172.17.0.1:53", "127.0.0.123:53"},
			want: []string{"127.0.0.123", "172.17.0.1"},
		},
		{
			name: "ipv6 bracketed",
			in:   []string{"[::1]:53"},
			want: []string{"::1"},
		},
		{
			name: "malformed entries are dropped",
			in:   []string{"127.0.0.123:53", "no-port-here", ":53", "good:53"},
			want: []string{"127.0.0.123", "good"},
		},
		{
			name: "empty input yields empty output",
			in:   nil,
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dnsServerIPs(c.in)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("dnsServerIPs(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
