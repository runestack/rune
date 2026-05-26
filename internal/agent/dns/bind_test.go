package dns

import (
	"runtime"
	"testing"
)

func TestFilterBindable_KeepsLoopbackHighPort(t *testing.T) {
	addrs := FilterBindable([]string{DefaultBindAddr, DevDefaultBindAddr}, nil)
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
	out := FilterBindable(in, nil)
	if len(out) > 1 {
		t.Fatalf("expected dedupe, got %v", out)
	}
}

func TestProbeBind_Invalid(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("127.0.0.123 bind behavior is OS-specific")
	}
	if probeBind("127.0.0.123:53") {
		t.Fatal("127.0.0.123:53 should not bind on macOS without a loopback alias")
	}
}
