// Package acme implements the asynchronous certificate issuance
// orchestrator for the ingress controller (RUNE-066).
//
// The orchestrator is intentionally split from any concrete ACME
// client implementation so the state machine is fully unit-testable
// against a fake Issuer, and so that a Pebble-backed integration
// test can exercise the production HTTP-01 path without touching
// state-machine code.
//
// Lifecycle of a single (service, host) request:
//
//	Pending --(Issuer.Issue ok)--> Issued
//	   |                              |
//	   |                              +-- (renewal cron, 30 days
//	   |                                  before expiry) --> Pending
//	   v
//	Failed --(NextRetry elapsed)-----> Pending
//
// Default-deny does not apply here; missing IngressCertStatus means
// no ACME work is requested. The orchestrator is single-writer per
// (namespace, name, host) tuple; concurrent requests are coalesced.
//
// **Multi-node note:** in v1 the orchestrator runs unconditionally
// because single-node = trivially the only node. Multi-node leader
// election is RUNE-066b; the LeaderProvider hook below makes the
// transition mechanical.
package acme

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// Issuer is the abstract certificate issuer. Production binds this
// to an HTTP-01 ACME client; tests bind it to a fake.
//
// Issue MUST be cancellable via ctx and SHOULD return promptly when
// it is. Implementations are responsible for performing the challenge
// dance (publishing the token via ChallengeStore where appropriate).
type Issuer interface {
	// Issue obtains a certificate for host. It returns the encoded
	// PEM cert chain and PEM private key on success.
	Issue(ctx context.Context, host string) (cert []byte, key []byte, err error)
}

// ChallengeStore is the seam through which the orchestrator publishes
// HTTP-01 challenge tokens. Edge ingress listeners read from the
// same store. The Issuer typically wraps this with the keyAuth value
// for the token; the listener just serves whatever is stored.
type ChallengeStore interface {
	// Put stores the keyAuth value for the given token. The token
	// path served by the ingress listener is
	// /.well-known/acme-challenge/<token>.
	Put(token, keyAuth string)
	// Delete removes the token after the challenge completes.
	Delete(token string)
}

// CertStore is where issued certificates land. The ingress listener
// reads here on every TLS handshake (or on hot-reload signal).
//
// Set MUST be atomic: a partial cert + missing key is a TLS
// handshake failure that the orchestrator should never produce.
type CertStore interface {
	// Set persists cert + key for host atomically.
	Set(ctx context.Context, host string, cert, key []byte) error
	// Get returns the most recent (cert, key) for host, or
	// (nil, nil, nil) if no cert exists yet.
	Get(ctx context.Context, host string) (cert, key []byte, err error)
	// Delete removes any cert for host.
	Delete(ctx context.Context, host string) error
}

// StatusSink is how the orchestrator surfaces IngressCertStatus
// back to the operator. The orchestrator does not own the Service
// object; the sink writes to whatever store the control plane uses.
type StatusSink interface {
	UpdateIngressCert(ctx context.Context, namespace, name string, status types.IngressCertStatus) error
}

// LeaderProvider gates whether this orchestrator instance should
// drive issuance. Single-node returns true unconditionally; the
// multi-node wrapper (RUNE-066b) plugs the Raft leader check here.
type LeaderProvider interface {
	IsLeader() bool
}

// AlwaysLeader is a LeaderProvider that always returns true. Used
// in single-node and in tests.
type AlwaysLeader struct{}

// IsLeader returns true.
func (AlwaysLeader) IsLeader() bool { return true }

// Request describes one (service, host) pair the orchestrator
// should keep alive — issue, renew, retry on failure.
type Request struct {
	Namespace string
	Name      string
	Host      string
}

// Key uniquely identifies a request inside the orchestrator's
// in-memory map. Includes namespace+name+host so two services in
// different namespaces can request the same host without collision
// (the operator's responsibility to make sure DNS only points one
// way at a time).
func (r Request) Key() string { return r.Namespace + "/" + r.Name + "/" + r.Host }

// RetryPolicy controls exponential backoff after issuance failure.
type RetryPolicy struct {
	// Initial is the first retry delay after a failure. Default 30s.
	Initial time.Duration
	// Max caps the retry delay. Default 1h per design doc.
	Max time.Duration
	// Multiplier between successive retries. Default 2.0.
	Multiplier float64
}

func (rp *RetryPolicy) defaults() {
	if rp.Initial <= 0 {
		rp.Initial = 30 * time.Second
	}
	if rp.Max <= 0 {
		rp.Max = time.Hour
	}
	if rp.Multiplier <= 1.0 {
		rp.Multiplier = 2.0
	}
}

// nextDelay returns the delay for the (1-based) attempt number.
// attempt=1 → Initial; attempt=N → min(Initial * mult^(N-1), Max).
func (rp RetryPolicy) nextDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := float64(rp.Initial)
	for i := 1; i < attempt; i++ {
		d *= rp.Multiplier
		if d >= float64(rp.Max) {
			return rp.Max
		}
	}
	if d > float64(rp.Max) {
		return rp.Max
	}
	return time.Duration(d)
}

// Config bundles the orchestrator's dependencies.
type Config struct {
	Issuer Issuer
	Certs  CertStore
	Status StatusSink
	Leader LeaderProvider
	Logger log.Logger
	Retry  RetryPolicy
	// RenewBefore is how long before ExpiresAt to renew. Default 30 days.
	RenewBefore time.Duration
	// Now overrides the clock for tests.
	Now func() time.Time
}

func (c *Config) defaults() {
	c.Retry.defaults()
	if c.RenewBefore <= 0 {
		c.RenewBefore = 30 * 24 * time.Hour
	}
	if c.Logger == nil {
		c.Logger = log.GetDefaultLogger().WithComponent("acme")
	}
	if c.Leader == nil {
		c.Leader = AlwaysLeader{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Orchestrator drives issuance, renewal, and retry for a set of
// Requests. Concurrency-safe.
type Orchestrator struct {
	cfg Config

	mu       sync.Mutex
	requests map[string]*requestState // keyed by Request.Key()
	wakeCh   chan struct{}            // signals the run loop
}

type requestState struct {
	req         Request
	status      types.IngressCertStatus
	failedCount int       // consecutive failures
	nextAction  time.Time // earliest time to attempt next state transition
}

// New constructs an Orchestrator. Issuer, Certs, and Status are
// required; New panics if any is nil — these are programming errors,
// not runtime conditions.
func New(cfg Config) *Orchestrator {
	if cfg.Issuer == nil {
		panic("acme: Issuer is required")
	}
	if cfg.Certs == nil {
		panic("acme: Certs is required")
	}
	if cfg.Status == nil {
		panic("acme: Status is required")
	}
	cfg.defaults()
	return &Orchestrator{
		cfg:      cfg,
		requests: make(map[string]*requestState),
		wakeCh:   make(chan struct{}, 1),
	}
}

// Submit registers (or refreshes) a request. Idempotent — calling
// Submit with the same key on an already-issued cert is a no-op
// until renewal time.
//
// On first registration of a host, Submit consults the CertStore for
// an already-persisted cert and, when one exists and is still well
// away from expiry, transitions the request directly to Issued (next
// action = renewal point). This is the load-bearing fix for the
// production outage where a runed restart re-issued every cert from
// scratch and tripped the Let's Encrypt per-identifier-set rate
// limit. See RUNE-BUG-ACME-REISSUE-ON-EVERY-RESTART.
func (o *Orchestrator) Submit(req Request) {
	if req.Host == "" {
		return
	}
	o.mu.Lock()
	if _, ok := o.requests[req.Key()]; ok {
		// Already tracked.
		o.mu.Unlock()
		return
	}
	now := o.cfg.Now()
	state := &requestState{
		req: req,
		status: types.IngressCertStatus{
			Host:  req.Host,
			State: types.IngressCertPending,
		},
		nextAction: now,
	}
	o.requests[req.Key()] = state
	o.mu.Unlock()

	// Probe the persistent CertStore for an existing cert outside the
	// orchestrator mutex (Get may touch disk / a transactional store).
	// A missing cert (nil / nil) or a parse failure falls through to
	// the existing Pending → attemptIssue path. A live, non-expired
	// cert short-circuits straight to Issued.
	if cert, _, err := o.cfg.Certs.Get(context.Background(), req.Host); err == nil && len(cert) > 0 {
		if expiry, perr := certNotAfter(cert); perr == nil && expiry.After(now.Add(o.cfg.RenewBefore)) {
			o.mu.Lock()
			state.status.State = types.IngressCertIssued
			state.status.IssuedAt = nil // we don't know when LE issued it; leave nil
			state.status.ExpiresAt = &expiry
			state.status.LastError = ""
			state.status.NextRetry = nil
			state.nextAction = expiry.Add(-o.cfg.RenewBefore)
			o.mu.Unlock()
			o.cfg.Logger.Info("acme cert reused from persistent store",
				log.Str("host", req.Host),
				log.Str("namespace", req.Namespace),
				log.Str("name", req.Name),
				log.Time("expiresAt", expiry))
			_ = o.cfg.Status.UpdateIngressCert(context.Background(), req.Namespace, req.Name, snapshotStatus(state, &o.mu))
		}
	}
	o.wake()
}

// Forget removes a request. Existing cert in the CertStore is left
// alone (deletion is a separate concern owned by the controller
// that drives orchestrator lifecycle).
func (o *Orchestrator) Forget(req Request) {
	o.mu.Lock()
	delete(o.requests, req.Key())
	o.mu.Unlock()
}

// Status returns a snapshot of the current status for req, or
// (zero, false) if not tracked.
func (o *Orchestrator) Status(req Request) (types.IngressCertStatus, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.requests[req.Key()]
	if !ok {
		return types.IngressCertStatus{}, false
	}
	return s.status, true
}

// wake signals the run loop. Non-blocking; coalesces.
func (o *Orchestrator) wake() {
	select {
	case o.wakeCh <- struct{}{}:
	default:
	}
}

// Run drives the state machine until ctx is done. Safe to call only
// once; the orchestrator is not designed to restart in the same
// process.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.cfg.Logger.Info("acme orchestrator starting")
	defer o.cfg.Logger.Info("acme orchestrator stopped")
	for {
		next := o.tick(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Sleep until the soonest scheduled action or until woken.
		var timer *time.Timer
		var timerCh <-chan time.Time
		if !next.IsZero() {
			d := next.Sub(o.cfg.Now())
			if d < 0 {
				d = 0
			}
			timer = time.NewTimer(d)
			timerCh = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-o.wakeCh:
			if timer != nil {
				timer.Stop()
			}
		case <-timerCh:
		}
	}
}

// tick processes every request whose nextAction has elapsed. Returns
// the soonest future nextAction across all requests, or zero time
// if there is no pending work.
func (o *Orchestrator) tick(ctx context.Context) time.Time {
	if !o.cfg.Leader.IsLeader() {
		// Re-check in 5s when not leader.
		return o.cfg.Now().Add(5 * time.Second)
	}
	now := o.cfg.Now()
	o.mu.Lock()
	work := make([]*requestState, 0, len(o.requests))
	for _, s := range o.requests {
		if !s.nextAction.IsZero() && !s.nextAction.After(now) {
			work = append(work, s)
		}
	}
	o.mu.Unlock()

	for _, s := range work {
		o.process(ctx, s)
		if ctx.Err() != nil {
			return time.Time{}
		}
	}

	// Compute soonest nextAction.
	o.mu.Lock()
	defer o.mu.Unlock()
	var soonest time.Time
	for _, s := range o.requests {
		if s.nextAction.IsZero() {
			continue
		}
		if soonest.IsZero() || s.nextAction.Before(soonest) {
			soonest = s.nextAction
		}
	}
	return soonest
}

// process performs one state transition for s. It deliberately does
// not hold the orchestrator mutex while calling the Issuer (network
// I/O) or the StatusSink.
func (o *Orchestrator) process(ctx context.Context, s *requestState) {
	o.mu.Lock()
	state := s.status.State
	host := s.req.Host
	ns, name := s.req.Namespace, s.req.Name
	o.mu.Unlock()

	switch state {
	case types.IngressCertPending, types.IngressCertFailed:
		o.attemptIssue(ctx, s, host, ns, name)
	case types.IngressCertIssued:
		o.maybeRenew(s, host)
	default:
		// Unknown — push to Pending.
		o.mu.Lock()
		s.status.State = types.IngressCertPending
		s.nextAction = o.cfg.Now()
		o.mu.Unlock()
	}
}

func (o *Orchestrator) attemptIssue(ctx context.Context, s *requestState, host, ns, name string) {
	cert, key, err := o.cfg.Issuer.Issue(ctx, host)
	now := o.cfg.Now()
	if err != nil {
		o.mu.Lock()
		s.failedCount++
		delay := o.cfg.Retry.nextDelay(s.failedCount)
		next := now.Add(delay)
		s.status.State = types.IngressCertFailed
		s.status.LastError = err.Error()
		s.status.NextRetry = &next
		s.nextAction = next
		o.mu.Unlock()
		o.cfg.Logger.Warn("acme issuance failed",
			log.Str("host", host),
			log.Str("namespace", ns),
			log.Str("name", name),
			log.Int("attempt", s.failedCount),
			log.Duration("retryIn", delay),
			log.Err(err))
		_ = o.cfg.Status.UpdateIngressCert(ctx, ns, name, snapshotStatus(s, &o.mu))
		return
	}
	if err := o.cfg.Certs.Set(ctx, host, cert, key); err != nil {
		o.mu.Lock()
		s.failedCount++
		delay := o.cfg.Retry.nextDelay(s.failedCount)
		next := now.Add(delay)
		s.status.State = types.IngressCertFailed
		s.status.LastError = "store cert: " + err.Error()
		s.status.NextRetry = &next
		s.nextAction = next
		o.mu.Unlock()
		o.cfg.Logger.Warn("acme cert store failed",
			log.Str("host", host),
			log.Err(err))
		_ = o.cfg.Status.UpdateIngressCert(ctx, ns, name, snapshotStatus(s, &o.mu))
		return
	}
	expiry, parseErr := certNotAfter(cert)
	o.mu.Lock()
	s.failedCount = 0
	s.status.State = types.IngressCertIssued
	s.status.LastError = ""
	s.status.NextRetry = nil
	issuedAt := now
	s.status.IssuedAt = &issuedAt
	if parseErr == nil {
		s.status.ExpiresAt = &expiry
		// Schedule renewal RenewBefore prior to expiry.
		renewAt := expiry.Add(-o.cfg.RenewBefore)
		if renewAt.Before(now.Add(time.Hour)) {
			renewAt = now.Add(time.Hour) // never tighter than 1h after issue
		}
		s.nextAction = renewAt
	} else {
		// Couldn't parse; renew in 24h conservatively.
		s.nextAction = now.Add(24 * time.Hour)
	}
	o.mu.Unlock()
	o.cfg.Logger.Info("acme cert issued",
		log.Str("host", host),
		log.Str("namespace", ns),
		log.Str("name", name),
		log.Time("expiresAt", expiry))
	_ = o.cfg.Status.UpdateIngressCert(ctx, ns, name, snapshotStatus(s, &o.mu))
}

func (o *Orchestrator) maybeRenew(s *requestState, host string) {
	o.mu.Lock()
	now := o.cfg.Now()
	if s.status.ExpiresAt == nil || now.Before(s.status.ExpiresAt.Add(-o.cfg.RenewBefore)) {
		// Not yet time. Reschedule to the renewal point.
		if s.status.ExpiresAt != nil {
			s.nextAction = s.status.ExpiresAt.Add(-o.cfg.RenewBefore)
		} else {
			s.nextAction = now.Add(24 * time.Hour)
		}
		o.mu.Unlock()
		return
	}
	// Renewal time. Drop back to Pending so attemptIssue runs.
	s.status.State = types.IngressCertPending
	s.nextAction = now
	o.mu.Unlock()
	o.cfg.Logger.Info("acme cert renewal triggered", log.Str("host", host))
	o.wake()
}

// snapshotStatus returns a copy of s.status while holding mu (which
// the caller must NOT hold). It exists so callers can release the
// orchestrator mutex before calling out to the StatusSink.
func snapshotStatus(s *requestState, mu *sync.Mutex) types.IngressCertStatus {
	mu.Lock()
	defer mu.Unlock()
	return s.status
}

// certNotAfter parses the first certificate from a PEM bundle and
// returns its NotAfter.
func certNotAfter(certPEM []byte) (time.Time, error) {
	return parseFirstCertNotAfter(certPEM)
}

// errNoCert is returned when a PEM bundle contains no certificates.
var errNoCert = errors.New("acme: PEM bundle contained no certificate")

// String satisfies the fmt.Stringer interface for log fields.
func (r Request) String() string {
	return fmt.Sprintf("%s/%s host=%s", r.Namespace, r.Name, r.Host)
}
