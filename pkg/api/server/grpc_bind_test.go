package server

import (
	"testing"

	"github.com/runestack/rune/pkg/log"
)

func TestGRPCBindAddr(t *testing.T) {
	cases := []struct {
		name       string
		addr       string
		tls        bool
		requireTLS bool
		want       string
	}{
		{"plaintext public, not required: serves as asked", ":7863", false, false, ":7863"},
		{"plaintext public, required: loopback", ":7863", false, true, "127.0.0.1:7863"},
		{"plaintext specific host, required: loopback", "10.0.0.5:7863", false, true, "127.0.0.1:7863"},
		{"plaintext loopback, required: unchanged", "127.0.0.1:7863", false, true, "127.0.0.1:7863"},
		{"TLS public, required: unchanged", "0.0.0.0:7863", true, true, "0.0.0.0:7863"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &APIServer{
				options: &Options{GRPCAddr: tc.addr, EnableTLS: tc.tls, GRPCRequireTLS: tc.requireTLS},
				logger:  log.NewTestLogger(),
			}
			if got := s.grpcBindAddr(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
