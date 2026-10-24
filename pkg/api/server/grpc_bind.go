package server

import (
	"net"
	"strings"

	"github.com/runestack/rune/pkg/log"
)

// grpcBindAddr applies the TLS posture to the gRPC listener. With TLS off on a
// non-loopback address it binds loopback when GRPCRequireTLS is set, and
// otherwise serves as asked but warns, since every bearer token and revealed
// secret on that listener crosses the network in clear.
func (s *APIServer) grpcBindAddr() string {
	addr := s.options.GRPCAddr
	if s.options.EnableTLS || isLoopbackAddr(addr) {
		return addr
	}
	if s.options.GRPCRequireTLS {
		loopback := "127.0.0.1:" + portOf(addr)
		s.logger.Warn("server require_tls is set but TLS is disabled; binding gRPC to loopback only",
			log.Str("requested", addr), log.Str("bound", loopback))
		return loopback
	}
	s.logger.Warn("gRPC is serving plaintext on a non-loopback address: bearer tokens and secrets cross the network in clear. Set [server.tls] enabled = true",
		log.Str("address", addr))
	return addr
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return strings.TrimPrefix(addr, ":")
}
