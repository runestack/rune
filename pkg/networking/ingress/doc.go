// Package ingress implements the edge-node ingress controller for
// RUNE-066. It owns :80 and :443 on edge nodes and:
//
//   - serves HTTP-01 ACME challenges so the orchestrator can complete
//     certificate issuance against Let's Encrypt;
//   - routes plain HTTP and TLS-terminated HTTPS traffic to backend
//     services by Host header;
//   - hot-reloads TLS certificates without restart via
//     tls.Config.GetCertificate.
//
// The package is split into:
//
//   - router.go      — host -> Route table; thread-safe Apply / Match
//   - challenge.go   — in-memory ChallengeStore for ACME HTTP-01
//   - cert_loader.go — CertStore-backed dynamic TLS GetCertificate
//   - server.go      — HTTP + HTTPS net.Listener Subsystem
//
// Single-node v1 deliberately defers nftables / IPVS path; the
// listener handles requests directly. Performance is acceptable
// because TLS termination is local CPU-bound work, and the only
// upstream hop is a localhost dial to the data-plane proxy.
package ingress
