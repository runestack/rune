package ingress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/networking/acme"
)

// generateSelfSignedCert returns a 1-day cert + key for host as PEM.
func generateSelfSignedCert(t *testing.T, host string) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{host},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestServer_ChallengeServed(t *testing.T) {
	r := NewRouter()
	store := NewMemChallengeStore()
	store.Put("tok-123", "key-auth-value")
	certs := NewCertLoader(acme.NewMemCertStore())
	sub, err := New(Config{
		Router:           r,
		Challenges:       store,
		Certs:            certs,
		HTTPAddr:         "127.0.0.1:0",
		HTTPSAddr:        "",
		UpstreamResolver: FuncResolver(func(string, string, int) (string, bool) { return "", false }),
		Logger:           log.GetDefaultLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sub.Stop(context.Background())
	<-sub.Ready()

	resp, err := http.Get("http://" + sub.HTTPAddr() + "/.well-known/acme-challenge/tok-123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "key-auth-value" {
		t.Fatalf("body=%q", body)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestServer_ProxyByHost(t *testing.T) {
	// Backend that echoes the X-Forwarded headers.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s xfh=%s xfp=%s", r.Host, r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Proto"))
	}))
	defer backend.Close()

	bhost, bport, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(bport)

	r := NewRouter()
	r.Apply([]Route{{Host: "api.example.com", Namespace: "prod", Service: "api", Port: port}})

	resolver := FuncResolver(func(ns, svc string, p int) (string, bool) {
		if ns == "prod" && svc == "api" && p == port {
			return net.JoinHostPort(bhost, strconv.Itoa(port)), true
		}
		return "", false
	})

	sub, err := New(Config{
		Router:           r,
		Challenges:       NewMemChallengeStore(),
		Certs:            NewCertLoader(acme.NewMemCertStore()),
		HTTPAddr:         "127.0.0.1:0",
		HTTPSAddr:        "",
		UpstreamResolver: resolver,
		Logger:           log.GetDefaultLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer sub.Stop(context.Background())

	req, _ := http.NewRequest("GET", "http://"+sub.HTTPAddr()+"/", nil)
	req.Host = "api.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "xfh=api.example.com") {
		t.Fatalf("body=%q", body)
	}
	if !strings.Contains(string(body), "xfp=http") {
		t.Fatalf("body=%q (proto)", body)
	}
}

func TestServer_NoRoute_404(t *testing.T) {
	sub, err := New(Config{
		Router:           NewRouter(),
		Challenges:       NewMemChallengeStore(),
		Certs:            NewCertLoader(acme.NewMemCertStore()),
		HTTPAddr:         "127.0.0.1:0",
		HTTPSAddr:        "",
		UpstreamResolver: FuncResolver(func(string, string, int) (string, bool) { return "", false }),
		Logger:           log.GetDefaultLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer sub.Stop(context.Background())

	resp, err := http.Get("http://" + sub.HTTPAddr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestServer_TLSHotReload(t *testing.T) {
	store := acme.NewMemCertStore()
	cert1, key1 := generateSelfSignedCert(t, "tls.example.com")
	if err := store.Set(context.Background(), "tls.example.com", cert1, key1); err != nil {
		t.Fatal(err)
	}
	loader := NewCertLoader(store)

	r := NewRouter()
	r.Apply([]Route{{Host: "tls.example.com", Namespace: "n", Service: "s", Port: 1}})

	var calls atomic.Int64
	resolver := FuncResolver(func(string, string, int) (string, bool) {
		calls.Add(1)
		return "127.0.0.1:1", false
	})

	sub, err := New(Config{
		Router:           r,
		Challenges:       NewMemChallengeStore(),
		Certs:            loader,
		HTTPAddr:         "127.0.0.1:0",
		HTTPSAddr:        "127.0.0.1:0",
		UpstreamResolver: resolver,
		Logger:           log.GetDefaultLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer sub.Stop(context.Background())

	// Connect via TLS, verify SNI returns cert1.
	dial := func() *x509.Certificate {
		conn, err := tls.Dial("tcp", sub.HTTPSAddr(), &tls.Config{
			ServerName:         "tls.example.com",
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		state := conn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			t.Fatal("no peer certs")
		}
		return state.PeerCertificates[0]
	}
	c1 := dial()
	if c1.SerialNumber == nil {
		t.Fatal("no serial")
	}

	// Replace cert in the store; reload triggers hot-reload.
	cert2, key2 := generateSelfSignedCert(t, "tls.example.com")
	if err := store.Set(context.Background(), "tls.example.com", cert2, key2); err != nil {
		t.Fatal(err)
	}
	if err := loader.Reload(context.Background(), "tls.example.com"); err != nil {
		t.Fatal(err)
	}
	c2 := dial()
	if c1.SerialNumber.Cmp(c2.SerialNumber) == 0 {
		t.Fatalf("expected new cert serial after reload (still %v)", c1.SerialNumber)
	}
}
