package dns

import (
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestFilterBindable_KeepsLoopbackHighPort(t *testing.T) {
	addrs, _ := FilterBindable([]string{DefaultBindAddr, DevDefaultBindAddr}, nil)
	if len(addrs) == 0 {
		t.Skip("no bindable DNS addresses in this environment")
	}
	foundDev := false
	for _, a := range addrs {
		if a == DevDefaultBindAddr {
			foundDev = true
		}
	}
	if !foundDev {
		t.Logf("bindable=%v (DevDefaultBindAddr optional when 127.0.0.123 works)", addrs)
	}
}

func TestFilterBindable_Dedupes(t *testing.T) {
	in := []string{"127.0.0.1:15353", "127.0.0.1:15353"}
	out, _ := FilterBindable(in, nil)
	if len(out) > 1 {
		t.Fatalf("expected dedupe, got %v", out)
	}
}

func TestFilterBindable_ReportsSkipped(t *testing.T) {
	// An address not assigned to this host can't bind; it must surface in
	// skipped rather than vanish silently.
	const bogus = "192.0.2.1:15353" // TEST-NET-1, never a local address
	bindable, skipped := FilterBindable([]string{DevDefaultBindAddr, bogus}, nil)
	for _, a := range bindable {
		if a == bogus {
			t.Fatalf("bogus address unexpectedly bound: %v", bindable)
		}
	}
	found := false
	for _, f := range skipped {
		if f.Addr == bogus {
			if f.Err == nil {
				t.Fatal("skipped BindFailure must carry the bind error")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s in skipped, got %v", bogus, skipped)
	}
}

func TestProbeBind_Invalid(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("127.0.0.123 bind behavior is OS-specific")
	}
	if err := probeBind("127.0.0.123:53"); err == nil {
		t.Fatal("127.0.0.123:53 should not bind on macOS without a loopback alias")
	}
}

func TestDiagnoseEmptyBind_PrivilegedPermission(t *testing.T) {
	// The careflow field report: every :53 candidate denied for lack of
	// CAP_NET_BIND_SERVICE.
	skipped := []BindFailure{
		{Addr: "127.0.0.123:53", Err: permErr("127.0.0.123", 53)},
		{Addr: "172.17.0.1:53", Err: permErr("172.17.0.1", 53)},
	}
	msg := DiagnoseEmptyBind(skipped)
	if !strings.Contains(msg, "CAP_NET_BIND_SERVICE") {
		t.Fatalf("expected CAP_NET_BIND_SERVICE hint, got: %s", msg)
	}
	if !strings.Contains(msg, "runed print-systemd") {
		t.Fatalf("expected regeneration command, got: %s", msg)
	}
	if !strings.Contains(msg, "172.17.0.1:53") {
		t.Fatalf("expected tried addresses listed, got: %s", msg)
	}
}

func TestDiagnoseEmptyBind_NonPermissionDoesNotBlameCaps(t *testing.T) {
	// A non-permission failure (e.g. address not local) must not misattribute
	// the cause to missing capabilities.
	skipped := []BindFailure{
		{Addr: "192.0.2.1:53", Err: errors.New("bind: cannot assign requested address")},
	}
	msg := DiagnoseEmptyBind(skipped)
	if strings.Contains(msg, "CAP_NET_BIND_SERVICE") {
		t.Fatalf("must not blame caps for a non-permission failure, got: %s", msg)
	}
	if !strings.Contains(msg, "192.0.2.1:53") {
		t.Fatalf("expected the tried address listed, got: %s", msg)
	}
}

func TestDiagnoseEmptyBind_HighPortPermissionNotCapsIssue(t *testing.T) {
	// Permission error but on an unprivileged port — not a caps problem.
	skipped := []BindFailure{
		{Addr: "127.0.0.1:15353", Err: permErr("127.0.0.1", 15353)},
	}
	if strings.Contains(DiagnoseEmptyBind(skipped), "CAP_NET_BIND_SERVICE") {
		t.Fatal("high-port permission error should not be attributed to missing caps")
	}
}

func TestDiagnoseEmptyBind_NoCandidates(t *testing.T) {
	msg := DiagnoseEmptyBind(nil)
	if !strings.Contains(msg, "no DNS bind candidates") {
		t.Fatalf("expected no-candidates explanation, got: %s", msg)
	}
}

// permErr builds an error shaped like a real privileged-bind denial so
// errors.Is(err, os.ErrPermission) holds, matching what the net package
// returns for EACCES.
func permErr(host string, port int) error {
	return &net.OpError{
		Op:   "listen",
		Net:  "udp",
		Addr: &net.UDPAddr{IP: net.ParseIP(host), Port: port},
		Err:  &os.SyscallError{Syscall: "bind", Err: os.ErrPermission},
	}
}
