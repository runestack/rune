package main

import (
	"reflect"
	"testing"
)

// TestDnsServerIPs documents the host:port → host transformation used
// to feed docker's --dns flag. It returns only container-reachable IPs:
// loopback binds are dropped (inside a container 127.x.x.x is the
// container's own loopback, a dead resolver entry), and non-IP hosts
// are dropped (docker --dns wants addresses; Rune always emits IPs).
func TestDnsServerIPs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "loopback bind is dropped (unreachable from containers)",
			in:   []string{"127.0.0.123:53"},
			want: []string{},
		},
		{
			name: "loopback dropped, bridge gateway kept",
			in:   []string{"127.0.0.123:53", "172.17.0.1:53", "127.0.0.123:53"},
			want: []string{"172.17.0.1"},
		},
		{
			name: "ipv6 loopback is dropped",
			in:   []string{"[::1]:53"},
			want: []string{},
		},
		{
			name: "multiple bridge gateways preserve order and dedupe",
			in:   []string{"172.17.0.1:53", "172.18.0.1:53", "172.17.0.1:53"},
			want: []string{"172.17.0.1", "172.18.0.1"},
		},
		{
			name: "malformed and non-IP entries are dropped",
			in:   []string{"172.17.0.1:53", "no-port-here", ":53", "good:53"},
			want: []string{"172.17.0.1"},
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
