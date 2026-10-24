package startup

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/crypto"
	"github.com/runestack/rune/pkg/log"
)

// serverTLSOptions maps [server.tls] and require_tls onto server options.
// TLS that is enabled but cannot be loaded is a startup error: falling back
// to plaintext would send credentials in clear while the operator believes
// they are protected.
func serverTLSOptions(t config.TLS, requireTLS bool, dataDir string, logger log.Logger) ([]server.Option, error) {
	opts := []server.Option{server.WithGRPCRequireTLS(requireTLS)}
	if !t.Enabled {
		if t.CertFile != "" || t.KeyFile != "" {
			logger.Warn("[server.tls] names certificate files but enabled = false; serving without TLS")
		}
		return opts, nil
	}

	var certFile, keyFile, fingerprint string
	switch {
	case t.CertFile != "" && t.KeyFile != "":
		if _, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile); err != nil {
			return nil, fmt.Errorf("[server.tls]: %w", err)
		}
		fp, err := crypto.CertFileFingerprint(t.CertFile)
		if err != nil {
			return nil, fmt.Errorf("[server.tls]: %w", err)
		}
		certFile, keyFile, fingerprint = t.CertFile, t.KeyFile, fp
	case t.CertFile != "" || t.KeyFile != "":
		return nil, errors.New("[server.tls] needs both cert_file and key_file, or neither for a self-signed certificate")
	default:
		sc, err := crypto.LoadOrCreateServerCert(filepath.Join(dataDir, "tls"), serverCertHosts(t.Hosts), time.Now())
		if err != nil {
			return nil, fmt.Errorf("self-signed TLS certificate: %w", err)
		}
		if sc.NewKey {
			logger.Info("Generated a self-signed TLS key", log.Str("key", sc.KeyFile))
		}
		certFile, keyFile, fingerprint = sc.CertFile, sc.KeyFile, sc.Fingerprint
	}
	logger.Info("TLS enabled for gRPC and the dashboard",
		log.Str("cert", certFile), log.Str("fingerprint", fingerprint))
	return append(opts, server.WithTLS(certFile, keyFile)), nil
}

// serverCertHosts lists the names and addresses clients may dial: loopback,
// the hostname, every address on a non-loopback interface, and the
// operator's extras. Addresses on the loopback interface are skipped because
// the dataplane adds a service VIP there for every service, which would
// churn the certificate's SANs.
func serverCertHosts(extra []string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, ipn.IP.String())
			}
		}
	}
	return append(hosts, extra...)
}
