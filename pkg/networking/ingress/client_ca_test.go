package ingress

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestClientCARegistry_ConfigFor(t *testing.T) {
	reg := NewClientCARegistry()
	pool := x509.NewCertPool()
	reg.Set("Api.Example.com", pool) // mixed case → normalized

	base := &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil },
		MinVersion:     tls.VersionTLS12,
	}

	// Host with a registered pool → require+verify, CAs set, server cert carried over.
	cfg := reg.ConfigFor(&tls.ClientHelloInfo{ServerName: "api.example.com"}, base)
	if cfg == nil {
		t.Fatal("expected a per-SNI config for a registered host")
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs != pool {
		t.Error("ClientCAs pool not wired")
	}
	if cfg.GetCertificate == nil {
		t.Error("server-cert GetCertificate must be carried into the clone")
	}
	// base must be unmodified (clone, not mutate).
	if base.ClientAuth != tls.NoClientCert {
		t.Error("base config must not be mutated")
	}

	// Unregistered host → nil (TLS stack keeps the base config, no client auth).
	if reg.ConfigFor(&tls.ClientHelloInfo{ServerName: "other.example.com"}, base) != nil {
		t.Error("unregistered host should yield nil (use base config)")
	}

	// Forget disables mTLS for the host.
	reg.Forget("api.example.com")
	if reg.ConfigFor(&tls.ClientHelloInfo{ServerName: "api.example.com"}, base) != nil {
		t.Error("after Forget, host should yield nil")
	}
}

func TestClientCARegistry_NilSafe(t *testing.T) {
	var reg *ClientCARegistry
	if reg.ConfigFor(&tls.ClientHelloInfo{ServerName: "x"}, &tls.Config{}) != nil {
		t.Error("nil registry must be nil-safe and yield nil")
	}
}
