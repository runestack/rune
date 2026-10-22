package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	uffdioAPI      = 0xc018aa3f // _IOWR(0xAA, 0x3F, struct uffdio_api)
	uffdioRegister = 0xc020aa00 // _IOWR(0xAA, 0x00, struct uffdio_register)
	uffdAPI        = 0xaa
	featEventRem   = 1 << 3 // UFFD_FEATURE_EVENT_REMOVE
	regModeMissing = 1
)

// TestMain lets the test binary double as the handler, so the handler runs in
// its own process as it does next to Firecracker. In process, a garbage
// collection could stop the handler goroutine while the faulting goroutine
// waits on it, and deadlock.
func TestMain(m *testing.M) {
	if os.Getenv("UFFD_HANDLER_CHILD") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestHandlerServesFaults plays Firecracker's side of the protocol: it
// registers an anonymous region with a userfaultfd, sends the region JSON and
// the fd to the handler over its socket, then reads the region and checks
// every page matches the memory file. It then drops a page with
// MADV_DONTNEED (as the balloon does) and checks it comes back zeroed.
// Needs root or vm.unprivileged_userfaultfd=1; skipped otherwise.
func TestHandlerServesFaults(t *testing.T) {
	const pages = 16
	ps := os.Getpagesize()
	size := pages * ps
	dir := t.TempDir()

	uffd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK, 0, 0)
	if errno != 0 {
		t.Skipf("userfaultfd unavailable: %v", errno)
	}
	defer unix.Close(int(uffd))
	api := [3]uint64{uffdAPI, featEventRem, 0}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uffd, uffdioAPI, uintptr(unsafe.Pointer(&api))); e != 0 {
		t.Skipf("UFFDIO_API with EVENT_REMOVE: %v", e)
	}

	// The snapshot memory file: page i is filled with byte i+1.
	want := make([]byte, size)
	for i := range pages {
		for j := range ps {
			want[i*ps+j] = byte(i + 1)
		}
	}
	memPath := filepath.Join(dir, "mem")
	if err := os.WriteFile(memPath, want, 0o600); err != nil {
		t.Fatal(err)
	}

	guest, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(guest)
	base := uint64(uintptr(unsafe.Pointer(&guest[0])))
	reg := [4]uint64{base, uint64(size), regModeMissing, 0}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uffd, uffdioRegister, uintptr(unsafe.Pointer(&reg))); e != 0 {
		t.Fatalf("UFFDIO_REGISTER: %v", e)
	}

	sock := filepath.Join(dir, "uffd.sock")
	statsPath := filepath.Join(dir, "stats.json")
	cmd := exec.Command(os.Args[0], "-sock", sock, "-mem", memPath, "-stats", statsPath)
	cmd.Env = append(os.Environ(), "UFFD_HANDLER_CHILD=1")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	var conn net.Conn
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if conn, err = net.Dial("unix", sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("handler socket: %v", err)
		}
	}
	defer conn.Close()
	body, _ := json.Marshal([]region{{Base: base, Size: uint64(size), Offset: 0, PageSize: uint64(ps)}})
	if _, _, err := conn.(*net.UnixConn).WriteMsgUnix(body, unix.UnixRights(int(uffd)), nil); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, size)
	copy(got, guest) // every page faults and is served by the handler
	if !bytes.Equal(got, want) {
		t.Fatal("guest memory does not match the memory file")
	}

	// Balloon-style removal of page 3: the next touch must see zeros, not the snapshot.
	page3 := guest[3*ps : 4*ps]
	if err := unix.Madvise(page3, unix.MADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page3, make([]byte, ps)) {
		t.Errorf("page 3 after MADV_DONTNEED = %x..., want zeros", page3[:8])
	}
	if !bytes.Equal(guest[4*ps:5*ps], want[4*ps:5*ps]) {
		t.Error("page 4 changed after removing page 3")
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	var st map[string]uint64
	b, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	t.Logf("handler stats: %s", b)
	// The handler counts after the ioctl has already woken the faulting
	// thread, so the last page may not be counted yet when SIGTERM lands.
	if st["copied_pages"] < pages-1 || st["remove_events"] == 0 || st["zero_pages"] == 0 {
		t.Errorf("unexpected stats %v", st)
	}
}
