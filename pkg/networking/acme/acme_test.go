package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// fakeIssuer programs sequence of (cert, err) responses for testing.
type fakeIssuer struct {
	mu      sync.Mutex
	scripts []fakeScript
	calls   atomic.Int64
}

type fakeScript struct {
	cert []byte
	key  []byte
	err  error
}

func (f *fakeIssuer) push(s fakeScript) {
	f.mu.Lock()
	f.scripts = append(f.scripts, s)
	f.mu.Unlock()
}

func (f *fakeIssuer) Issue(_ context.Context, _ string) ([]byte, []byte, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scripts) == 0 {
		return nil, nil, errors.New("fake: no script")
	}
	s := f.scripts[0]
	f.scripts = f.scripts[1:]
	return s.cert, s.key, s.err
}

type fakeStatus struct {
	mu      sync.Mutex
	last    map[string]types.IngressCertStatus
	updates int
}

func newFakeStatus() *fakeStatus {
	return &fakeStatus{last: map[string]types.IngressCertStatus{}}
}

func (f *fakeStatus) UpdateIngressCert(_ context.Context, ns, name string, st types.IngressCertStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[ns+"/"+name] = st
	f.updates++
	return nil
}

// makeCert returns a self-signed PEM cert valid until notAfter.
func makeCert(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func makeKey(t *testing.T) []byte {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func newOrch(t *testing.T, iss Issuer, certs CertStore, status StatusSink, now func() time.Time) *Orchestrator {
	t.Helper()
	return New(Config{
		Issuer: iss, Certs: certs, Status: status,
		Logger: log.GetDefaultLogger(),
		Now:    now,
		Retry:  RetryPolicy{Initial: 100 * time.Millisecond, Max: time.Second, Multiplier: 2},
	})
}

// runOnce executes a tick synchronously instead of going through Run.
func tick(o *Orchestrator) { o.tick(context.Background()) }

func TestOrchestrator_PendingToIssued(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	cert := makeCert(t, now.Add(60*24*time.Hour))
	key := makeKey(t)
	iss := &fakeIssuer{}
	iss.push(fakeScript{cert: cert, key: key})
	st := newFakeStatus()
	store := NewMemCertStore()
	o := newOrch(t, iss, store, st, clock)
	req := Request{Namespace: "prod", Name: "api", Host: "api.example.com"}
	o.Submit(req)
	tick(o)
	got, _ := o.Status(req)
	if got.State != types.IngressCertIssued {
		t.Fatalf("state=%s", got.State)
	}
	if got.IssuedAt == nil || got.ExpiresAt == nil {
		t.Fatalf("missing timestamps: %+v", got)
	}
	c, k, _ := store.Get(context.Background(), "api.example.com")
	if c == nil || k == nil {
		t.Fatal("cert not stored")
	}
	if st.updates == 0 {
		t.Fatal("status sink not called")
	}
}

func TestOrchestrator_FailureBackoff(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	iss := &fakeIssuer{}
	iss.push(fakeScript{err: errors.New("dns lookup failed")})
	o := newOrch(t, iss, NewMemCertStore(), newFakeStatus(), clock)
	req := Request{Namespace: "n", Name: "s", Host: "h.example"}
	o.Submit(req)
	tick(o)
	got, _ := o.Status(req)
	if got.State != types.IngressCertFailed {
		t.Fatalf("state=%s", got.State)
	}
	if got.LastError == "" {
		t.Fatal("LastError empty")
	}
	if got.NextRetry == nil {
		t.Fatal("NextRetry nil")
	}
	if !got.NextRetry.After(now) {
		t.Fatalf("NextRetry not in future: %v vs now %v", got.NextRetry, now)
	}
}

func TestOrchestrator_RetryThenSuccess(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	cert := makeCert(t, now.Add(60*24*time.Hour))
	key := makeKey(t)
	iss := &fakeIssuer{}
	iss.push(fakeScript{err: errors.New("transient")})
	iss.push(fakeScript{cert: cert, key: key})
	o := newOrch(t, iss, NewMemCertStore(), newFakeStatus(), clock)
	req := Request{Namespace: "n", Name: "s", Host: "h.example"}
	o.Submit(req)
	tick(o)
	got, _ := o.Status(req)
	if got.State != types.IngressCertFailed {
		t.Fatalf("expected Failed, got %s", got.State)
	}
	// Advance clock past the retry window.
	clock = func() time.Time { return now.Add(time.Second) }
	o.cfg.Now = clock
	tick(o)
	got, _ = o.Status(req)
	if got.State != types.IngressCertIssued {
		t.Fatalf("expected Issued after retry, got %s (lastErr=%s)", got.State, got.LastError)
	}
}

func TestOrchestrator_RenewalScheduled(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	// Cert that expires in 31 days.
	expires := now.Add(31 * 24 * time.Hour)
	cert := makeCert(t, expires)
	key := makeKey(t)
	iss := &fakeIssuer{}
	iss.push(fakeScript{cert: cert, key: key})
	o := newOrch(t, iss, NewMemCertStore(), newFakeStatus(), clock)
	o.cfg.RenewBefore = 30 * 24 * time.Hour
	req := Request{Namespace: "n", Name: "s", Host: "h.example"}
	o.Submit(req)
	tick(o)
	o.mu.Lock()
	rs := o.requests[req.Key()]
	next := rs.nextAction
	o.mu.Unlock()
	// Renewal should be ~24h from now (expires - RenewBefore).
	delta := next.Sub(now)
	if delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("renewal scheduled at %v (delta %v); expected ~24h", next, delta)
	}
}

func TestOrchestrator_ForgetRemoves(t *testing.T) {
	o := newOrch(t, &fakeIssuer{}, NewMemCertStore(), newFakeStatus(), time.Now)
	req := Request{Namespace: "n", Name: "s", Host: "h.example"}
	o.Submit(req)
	if _, ok := o.Status(req); !ok {
		t.Fatal("not tracked after submit")
	}
	o.Forget(req)
	if _, ok := o.Status(req); ok {
		t.Fatal("still tracked after forget")
	}
}

func TestRetryPolicy_NextDelay(t *testing.T) {
	rp := RetryPolicy{Initial: time.Second, Max: 10 * time.Second, Multiplier: 2}
	rp.defaults()
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {4, 8 * time.Second},
		{5, 10 * time.Second}, {10, 10 * time.Second},
	}
	for _, c := range cases {
		got := rp.nextDelay(c.attempt)
		if got != c.want {
			t.Fatalf("attempt=%d got=%v want=%v", c.attempt, got, c.want)
		}
	}
}

func TestOrchestrator_RunCancels(t *testing.T) {
	o := newOrch(t, &fakeIssuer{}, NewMemCertStore(), newFakeStatus(), time.Now)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := o.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want DeadlineExceeded", err)
	}
}

func TestParseFirstCertNotAfter(t *testing.T) {
	want := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	pemBytes := makeCert(t, want)
	got, err := parseFirstCertNotAfter(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := parseFirstCertNotAfter([]byte("garbage")); err == nil {
		t.Fatal("expected error on garbage")
	}
}

// Ensure leader gating prevents work.
func TestOrchestrator_NotLeader_NoIssue(t *testing.T) {
	iss := &fakeIssuer{}
	iss.push(fakeScript{cert: makeCert(t, time.Now().Add(48*time.Hour)), key: makeKey(t)})
	o := New(Config{
		Issuer: iss, Certs: NewMemCertStore(), Status: newFakeStatus(),
		Logger: log.GetDefaultLogger(),
		Leader: notLeader{},
	})
	o.Submit(Request{Namespace: "n", Name: "s", Host: "h.example"})
	tick(o)
	if iss.calls.Load() != 0 {
		t.Fatalf("issuer called %d times while not leader", iss.calls.Load())
	}
}

type notLeader struct{}

func (notLeader) IsLeader() bool { return false }

// catch unused imports
var _ = fmt.Sprintf
