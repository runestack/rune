package vip

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/orderedlog"
)

func newOlog(t *testing.T) *orderedlog.BadgerBackend {
	t.Helper()
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(dir, "olog")).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	be := orderedlog.NewBadgerBackend(db, orderedlog.BackendOptions{
		Logger: log.GetDefaultLogger().WithComponent("test.olog"),
	})
	if err := be.Open(); err != nil {
		t.Fatalf("olog open: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

func newAllocator(t *testing.T, cidr string) *Allocator {
	t.Helper()
	a, err := New(newOlog(t), Options{CIDR: cidr, SkipRouteCheck: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return a
}

func TestAllocate_BasicAndIdempotent(t *testing.T) {
	a := newAllocator(t, "10.96.0.0/24")
	ctx := context.Background()

	ip1, err := a.Allocate(ctx, "svc-a")
	if err != nil {
		t.Fatalf("alloc a: %v", err)
	}
	if ip1.String() != "10.96.0.2" {
		t.Fatalf("first IP=%s, want 10.96.0.2 (skip .0/.1)", ip1)
	}
	ip1b, err := a.Allocate(ctx, "svc-a")
	if err != nil {
		t.Fatalf("alloc a (re): %v", err)
	}
	if !ip1.Equal(ip1b) {
		t.Fatalf("idempotent re-alloc returned %s, want %s", ip1b, ip1)
	}

	ip2, err := a.Allocate(ctx, "svc-b")
	if err != nil {
		t.Fatalf("alloc b: %v", err)
	}
	if ip2.String() != "10.96.0.3" {
		t.Fatalf("second IP=%s, want 10.96.0.3", ip2)
	}
}

func TestAllocate_Concurrent_Unique(t *testing.T) {
	a := newAllocator(t, "10.96.0.0/24")
	ctx := context.Background()

	const n = 50
	results := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip, err := a.Allocate(ctx, "svc-"+itoa(i))
			if err != nil {
				t.Errorf("alloc: %v", err)
				return
			}
			results <- ip.String()
		}(i)
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for r := range results {
		if seen[r] {
			t.Fatalf("duplicate IP %s", r)
		}
		seen[r] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique IPs, want %d", len(seen), n)
	}
}

func TestAllocate_CIDRExhausted(t *testing.T) {
	a := newAllocator(t, "10.96.0.0/30") // .0 net, .1 gw, .2, .3 bcast → 1 usable
	ctx := context.Background()
	if _, err := a.Allocate(ctx, "svc-1"); err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	_, err := a.Allocate(ctx, "svc-2")
	if err == nil || err.Error() == "" || !contains(err.Error(), "exhausted") {
		t.Fatalf("expected CIDR exhausted, got %v", err)
	}
}

func TestRelease_CooldownThenReuse(t *testing.T) {
	prev := ReleaseCooldown
	ReleaseCooldown = 200 * time.Millisecond
	t.Cleanup(func() { ReleaseCooldown = prev })

	a := newAllocator(t, "10.96.0.0/30")
	ctx := context.Background()
	ip1, err := a.Allocate(ctx, "svc-1")
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	if err := a.Release("svc-1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Immediately retrying should not allocate the released IP yet
	// because /30 has only one usable address and it's still in
	// cooldown.
	if _, err := a.Allocate(ctx, "svc-2"); err == nil {
		t.Fatal("expected exhausted before cooldown elapses")
	}

	// Wait for cooldown + worker tick.
	deadline := time.Now().Add(3 * time.Second)
	var ip2 net.IP
	for time.Now().Before(deadline) {
		ip2, err = a.Allocate(ctx, "svc-2")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("alloc post-cooldown: %v", err)
	}
	if !ip2.Equal(ip1) {
		t.Fatalf("expected reuse of %s, got %s", ip1, ip2)
	}
}

func TestBootstrap_InvalidCIDR(t *testing.T) {
	cases := []string{
		"not-a-cidr",
		"8.8.8.0/24",       // public
		"2001:db8::/32",    // ipv6
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			a, err := New(newOlog(t), Options{CIDR: c, SkipRouteCheck: true})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer a.Close()
			if err := a.Bootstrap(context.Background()); err == nil {
				t.Fatalf("expected error bootstrapping %s", c)
			}
		})
	}
}

func TestBootstrap_Idempotent(t *testing.T) {
	olog := newOlog(t)
	a1, err := New(olog, Options{CIDR: "10.96.0.0/24", SkipRouteCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a1.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	a1.Close()

	// Re-bootstrap with same CIDR -> ok.
	a2, err := New(olog, Options{CIDR: "10.96.0.0/24", SkipRouteCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if err := a2.Bootstrap(context.Background()); err != nil {
		t.Fatalf("re-bootstrap same CIDR: %v", err)
	}

	// Re-bootstrap with different CIDR -> error.
	a3, err := New(olog, Options{CIDR: "172.20.0.0/16", SkipRouteCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Close()
	if err := a3.Bootstrap(context.Background()); err == nil {
		t.Fatal("expected ErrAlreadyBootstrapped on CIDR change")
	}
}

func TestStatus(t *testing.T) {
	a := newAllocator(t, "10.96.0.0/24")
	ctx := context.Background()
	for _, sid := range []string{"a", "b", "c"} {
		if _, err := a.Allocate(ctx, sid); err != nil {
			t.Fatal(err)
		}
	}
	st, pending := a.Status()
	if st.CIDR != "10.96.0.0/24" {
		t.Fatalf("CIDR=%s", st.CIDR)
	}
	if len(st.AllocatedVIPs) != 3 {
		t.Fatalf("allocated=%d, want 3", len(st.AllocatedVIPs))
	}
	if pending != 0 {
		t.Fatalf("pending=%d, want 0", pending)
	}
}

// --- tiny helpers (avoid pulling strconv/strings just for tests) ----

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
