// Package types: ingress certificate state.
//
// IngressCertStatus tracks the per-host TLS certificate lifecycle
// for services exposed via the ingress controller (RUNE-066).
// Certificate issuance is asynchronous: cast admits the service
// immediately, and the ACME orchestrator drives the state machine
// independently, updating IngressCertStatus as it makes progress.
package types

import "time"

// IngressCertState is a coarse-grained state for the certificate
// lifecycle. Operator-facing — keep the value space small.
type IngressCertState string

const (
	// IngressCertPending means the orchestrator has accepted the
	// request but no certificate has been issued yet. The service
	// is reachable over plain HTTP only.
	IngressCertPending IngressCertState = "Pending"

	// IngressCertIssued means a usable certificate is in the
	// secret store and edge nodes are serving it.
	IngressCertIssued IngressCertState = "Issued"

	// IngressCertFailed means the most recent issuance attempt
	// failed. The orchestrator will retry per NextRetry; LastError
	// carries operator-facing detail.
	IngressCertFailed IngressCertState = "Failed"
)

// IngressCertStatus is the per-service cert status surfaced to the
// operator via `rune get service`. Mirrors the networking-layer
// implementation plan (RUNE-066).
type IngressCertStatus struct {
	// Host is the hostname the cert is issued for, e.g. api.example.com.
	Host string `json:"host" yaml:"host"`

	// State is one of Pending, Issued, Failed.
	State IngressCertState `json:"state" yaml:"state"`

	// IssuedAt is set when State transitions to Issued.
	IssuedAt *time.Time `json:"issuedAt,omitempty" yaml:"issuedAt,omitempty"`

	// ExpiresAt is the certificate NotAfter from the issued cert.
	ExpiresAt *time.Time `json:"expiresAt,omitempty" yaml:"expiresAt,omitempty"`

	// LastError is populated when State == Failed.
	LastError string `json:"lastError,omitempty" yaml:"lastError,omitempty"`

	// NextRetry is set when State == Failed; the orchestrator
	// schedules the next attempt at this time.
	NextRetry *time.Time `json:"nextRetry,omitempty" yaml:"nextRetry,omitempty"`
}
